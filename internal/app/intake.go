package app

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

// effectiveRadius 把位置性质折算为最小传播跳数：靠近交叉道口至少跨 1 跳，
// 定位不确定时扩大到 2 跳，给清扫与塔台留出安全边界。
func effectiveRadius(qual string, requested int) int {
	min := 0
	switch qual {
	case "NEAR_INTERSECTION", "SPANS_SEGMENTS":
		min = 1
	case "UNCERTAIN":
		min = 2
	}
	if requested < min {
		return min
	}
	return requested
}

func startSegments(anchor, ref string, qual string) []string {
	starts := []string{anchor}
	if ref != "" && ref != anchor {
		switch qual {
		case "NEAR_INTERSECTION", "SPANS_SEGMENTS", "UNCERTAIN":
			starts = append(starts, ref)
		}
	}
	return starts
}

// candidate 表示关联候选事件及其评分。
type candidate struct {
	eventID string
	segment string
	ref     sql.NullString
	sig     sql.NullString
	score   float64
	basis   []string
}

// correlate 在所有未闭环事件中按位置与影像特征查找同一事件。
// 调用前必须已经 FOR UPDATE 锁住机场的未闭环事件。
func (s *Service) correlate(tx *sql.Tx, airportID string, in CreateReportInput) (*candidate, error) {
	rows, err := tx.Query(`
	 WITH cur AS (
	   SELECT e.id AS event_id,
	          (SELECT segment_id FROM event_versions f
	            WHERE f.event_id=e.id AND f.decision_state='FINAL'
	            ORDER BY version DESC LIMIT 1) AS seg,
	          (SELECT ref_segment_id FROM event_versions f
	            WHERE f.event_id=e.id AND f.decision_state='FINAL'
	            ORDER BY version DESC LIMIT 1) AS ref,
	          (SELECT closed_segments FROM event_versions f
	            WHERE f.event_id=e.id AND f.decision_state='FINAL'
	            ORDER BY version DESC LIMIT 1) AS closed,
	          e.image_sig
	     FROM fod_events e
	    WHERE e.airport_id=$1 AND e.closed=FALSE
	 )
	 SELECT event_id, seg, ref, image_sig, closed FROM cur`, airportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var best *candidate
	for rows.Next() {
		var c candidate
		var closed []string
		if err := rows.Scan(&c.eventID, &c.segment, &c.ref, &c.sig, pqArray(&closed)); err != nil {
			return nil, err
		}
		// 1) 影像特征：指纹一致（同一照片/同一物体）——强身份证据，足以独立认定同一事件
		if in.ImageSig != "" && c.sig.Valid && c.sig.String == in.ImageSig {
			c.score += 0.65
			c.basis = append(c.basis, "SIG_MATCH")
		}
		// 2) 位置：当前生效封闭集包含报告区段
		if c.segment == in.SegmentID {
			c.score += 0.35
			c.basis = append(c.basis, "SEGMENT_MATCH")
		} else if contains(closed, in.SegmentID) {
			c.score += 0.2
			c.basis = append(c.basis, "WITHIN_CLOSURE")
		}
		// 3) 交叉道口参照一致
		if in.RefSegmentID != "" && c.ref.Valid &&
			(c.ref.String == in.RefSegmentID || c.ref.String == in.SegmentID || c.segment == in.RefSegmentID) {
			c.score += 0.15
			c.basis = append(c.basis, "INTERSECTION_REF")
		}
		// 4) 同跑道相邻区段 + 影像近似（非空但不完全相同）给弱加分，不足以单独命中
		if c.score < 0.6 && c.segment != in.SegmentID && in.ImageSig != "" && c.sig.Valid &&
			hammingPrefix(in.ImageSig, c.sig.String) {
			c.score += 0.25
			c.basis = append(c.basis, "SIG_NEAR")
		}
		if c.score >= 0.6 && (best == nil || c.score > best.score) {
			cc := c
			best = &cc
		}
	}
	return best, rows.Err()
}

// hamningPrefix: 影像指纹近似（同长且前 2/3 字符一致视为近似）。
func hammingPrefix(a, b string) bool {
	if len(a) < 6 || len(a) != len(b) {
		return false
	}
	n := len(a) * 2 / 3
	return a[:n] == b[:n]
}

// SubmitReport 是报告入口：幂等、关联、必要时开新版本并传播封闭。
func (s *Service) SubmitReport(in CreateReportInput, actor Actor) (*ReportResult, error) {
	if in.ReportID == "" {
		in.ReportID = newID("RPT")
	}
	if in.Source == "" || in.SegmentID == "" || in.AirportID == "" {
		return nil, errf("BAD_REQUEST", "source/airport_id/segment_id 必填")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 幂等：重复报告 ID 直接返回首次关联结论，不再产生新版本/任务/通知。
	var existingEvent string
	var linked sql.NullBool
	err = tx.QueryRow(`SELECT event_id, linked_event_id IS NOT NULL FROM reports WHERE id=$1`, in.ReportID).
		Scan(&existingEvent, &linked)
	if err == nil {
		res, err := s.buildReportResult(tx, in.ReportID, existingEvent)
		if err != nil {
			return nil, err
		}
		res.ReportID = in.ReportID
		return res, tx.Commit()
	} else if err != sql.ErrNoRows {
		return nil, err
	}

	// 机场级串行点：锁住机场行。这样即使没有任何未闭环事件（锁空集不互斥），
	// 并发上报也会被串行化，杜绝同一异物被开出两个事件。
	var lockedAirport string
	if err := tx.QueryRow(`SELECT id FROM airports WHERE id=$1 FOR UPDATE`, in.AirportID).
		Scan(&lockedAirport); err != nil {
		if err == sql.ErrNoRows {
			return nil, errf("NOT_FOUND", "机场不存在")
		}
		return nil, err
	}
	// 同时锁定本机场全部未闭环事件行（后续版本滚动需要行锁）。
	lockRows, err := tx.Query(`SELECT id FROM fod_events WHERE airport_id=$1 AND closed=FALSE FOR UPDATE`, in.AirportID)
	if err != nil {
		return nil, err
	}
	lockRows.Close()

	best, err := s.correlate(tx, in.AirportID, in)
	if err != nil {
		return nil, err
	}

	reportTime := time.Now().UTC()
	if in.ReportTime != nil {
		reportTime = *in.ReportTime
	}

	if best != nil {
		return s.attachReport(tx, in, actor, best, reportTime)
	}
	return s.createEventFromReport(tx, in, actor, reportTime)
}

func (s *Service) buildReportResult(tx *sql.Tx, reportID, eventID string) (*ReportResult, error) {
	res := &ReportResult{ReportID: reportID, EventID: eventID}
	var score sql.NullFloat64
	var linkedEvent sql.NullString
	var basis sql.NullString
	err := tx.QueryRow(`SELECT linked_event_id, link_score, link_basis FROM reports WHERE id=$1`, reportID).
		Scan(&linkedEvent, &score, &basis)
	if err != nil {
		return nil, err
	}
	res.Linked = linkedEvent.Valid
	if score.Valid {
		res.LinkScore = score.Float64
	}
	if basis.Valid {
		res.LinkBasis = basis.String
	}
	var cur versionRow
	if err := tx.QueryRow(`SELECT id,event_id,version,change_kind,segment_id,ref_segment_id,location_qual,
	                risk_radius_seg,closed_segments,decision,decision_state
	         FROM event_versions WHERE event_id=$1 ORDER BY version DESC LIMIT 1`, eventID).
		Scan(&cur.ID, &cur.EventID, &cur.Version, &cur.ChangeKind, &cur.SegmentID,
			&cur.RefSegmentID, &cur.LocationQual, &cur.RiskRadius, pqArray(&cur.ClosedSegments), &cur.Decision, &cur.State); err != nil {
		return nil, err
	}
	res.NewVersion = cur.Version
	res.ClosedSegs = cur.ClosedSegments
	return res, nil
}

// attachReport 把报告挂到已有事件；若新定位证据扩大风险边界则开出新版本。
func (s *Service) attachReport(tx *sql.Tx, in CreateReportInput, actor Actor, best *candidate, reportTime time.Time) (*ReportResult, error) {
	// 事件行已在上面的 FOR UPDATE 集合中被锁定。
	_, err := tx.Exec(`SELECT id FROM fod_events WHERE id=$1 FOR UPDATE`, best.eventID)
	if err != nil {
		return nil, err
	}
	final, err := s.latestFinalVersion(tx, best.eventID)
	if err != nil {
		return nil, err
	}
	head, err := s.currentVersion(tx, best.eventID, true)
	if err != nil {
		return nil, err
	}

	g, err := s.loadAdjacency(tx, in.AirportID)
	if err != nil {
		return nil, err
	}
	radius := effectiveRadius(in.LocationQual, in.RiskRadius)
	newStarts := startSegments(in.SegmentID, in.RefSegmentID, in.LocationQual)
	newClosure, newEdges := closureSet(g, newStarts, radius)

	added, _ := diffSet(newClosure, final.ClosedSegments)
	anchorMoved := !contains(final.ClosedSegments, in.SegmentID)
	broader := in.LocationQual == "UNCERTAIN" && final.LocationQual != "UNCERTAIN"
	radiusGrew := radius > final.RiskRadius &&
		(in.SegmentID != final.SegmentID || in.RefSegmentID != final.RefSegmentID.String || in.LocationQual != final.LocationQual)
	expands := len(added) > 0 || anchorMoved || broader || radiusGrew

	// 插入报告（关联结论留痕）
	_, err = tx.Exec(`INSERT INTO reports
	 (id,event_id,source,reporter,report_time,segment_id,ref_segment_id,location_qual,photo_url,image_sig,
	  linked_event_id,link_score,link_basis,created_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,now())`,
		in.ReportID, best.eventID, in.Source, in.Reporter, reportTime, in.SegmentID,
		nullStr(in.RefSegmentID), in.LocationQual, nullStr(in.PhotoURL), nullStr(in.ImageSig),
		best.eventID, best.score, strings.Join(best.basis, "+"))
	if err != nil {
		return nil, err
	}
	s.audit(tx, "REPORT", in.ReportID, "LINKED", final.Version, in.Source, actor.UserID,
		map[string]interface{}{"event_id": best.eventID, "score": best.score, "basis": best.basis, "expands": expands})

	res := &ReportResult{
		ReportID: in.ReportID, EventID: best.eventID, Linked: true,
		LinkScore: best.score, LinkBasis: strings.Join(best.basis, "+"),
		NewVersion: head.Version, ClosedSegs: final.ClosedSegments, Expanded: false,
	}

	if !expands {
		// 同一风险边界内的补充报告：不开新版本，避免旧复查被无谓失效。
		return res, tx.Commit()
	}

	// ---- 扩大风险边界：旧的缩小/恢复提案立即作废，开出新的 CLOSED FINAL 版本 ----
	if head.State == "PENDING" {
		_, err = tx.Exec(`UPDATE event_versions SET decision_state='SUPERSEDED' WHERE id=$1`, head.ID)
		if err != nil {
			return nil, err
		}
	}
	maxVer := 0
	if err := tx.QueryRow(`SELECT max(version) FROM event_versions WHERE event_id=$1`, best.eventID).Scan(&maxVer); err != nil {
		return nil, err
	}
	newVer := maxVer + 1

	// 封闭集取并集：已封闭区段不解除，新增区段向外传播。
	union := append(append([]string{}, final.ClosedSegments...), added...)
	sort.Strings(union)
	union = dedup(union)
	// 以新证据锚点按新半径计算传播边；既有封闭区段以原传播路径为准（不重复扩张旧锚点）。
	_, unionEdges := closureSet(g, dedupSorted(newStarts), radius)

	// 更新事件锚点与影像指纹
	_, err = tx.Exec(`UPDATE fod_events SET segment_id=$2,ref_segment_id=$3,location_qual=$4,
	 image_sig=COALESCE(NULLIF($5,''),image_sig), photo_url=COALESCE(NULLIF($6,''),photo_url) WHERE id=$1`,
		best.eventID, in.SegmentID, nullStr(in.RefSegmentID), in.LocationQual, in.ImageSig, in.PhotoURL)
	if err != nil {
		return nil, err
	}

	var newID int64
	err = tx.QueryRow(`INSERT INTO event_versions
	 (event_id,version,change_kind,segment_id,ref_segment_id,location_qual,risk_radius_seg,
	  closed_segments,decision,decision_state,note,source,actor,created_at)
	 VALUES ($1,$2,'LOCATION_EXPAND',$3,$4,$5,$6,$7,'CLOSED','FINAL',$8,$9,$10,now()) RETURNING id`,
		best.eventID, newVer, in.SegmentID, nullStr(in.RefSegmentID), in.LocationQual, radius,
		pq.StringArray(union), fmt.Sprintf("定位证据扩大风险边界：新增 %v", added), in.Source, actor.UserID).
		Scan(&newID)
	if err != nil {
		return nil, err
	}
	if head.State == "PENDING" {
		_, _ = tx.Exec(`UPDATE event_versions SET superseded_by=$2 WHERE id=$1`, head.ID, newID)
	}

	if err := s.recordPropagation(tx, best.eventID, newVer, union, unionEdges, final.Version); err != nil {
		return nil, err
	}
	if err := s.replaceSegmentStatus(tx, best.eventID, newVer, union, "CLOSED", in.Source, actor.UserID); err != nil {
		return nil, err
	}
	if err := s.rolloverTasks(tx, best.eventID, final.Version, newVer, union, in.Source, actor.UserID, added); err != nil {
		return nil, err
	}
	if err := s.fanoutNotifications(tx, best.eventID, newVer, "CLOSED", union,
		"跑道异物风险边界扩大", in.Source, actor.UserID); err != nil {
		return nil, err
	}
	s.audit(tx, "EVENT", best.eventID, "EXPAND", newVer, in.Source, actor.UserID,
		map[string]interface{}{"added": added, "closure": union, "radius": radius,
			"superseded_pending_version": head.Version})

	res.NewVersion = newVer
	res.ClosedSegs = union
	res.Expanded = true
	res.Propagation = newEdges
	return res, tx.Commit()
}

func (s *Service) createEventFromReport(tx *sql.Tx, in CreateReportInput, actor Actor, reportTime time.Time) (*ReportResult, error) {
	eventID := newID("EVT")
	g, err := s.loadAdjacency(tx, in.AirportID)
	if err != nil {
		return nil, err
	}
	radius := effectiveRadius(in.LocationQual, in.RiskRadius)
	starts := startSegments(in.SegmentID, in.RefSegmentID, in.LocationQual)
	closure, propEdges := closureSet(g, starts, radius)

	_, err = tx.Exec(`INSERT INTO fod_events
	 (id,airport_id,segment_id,ref_segment_id,location_qual,photo_url,image_sig)
	 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		eventID, in.AirportID, in.SegmentID, nullStr(in.RefSegmentID), in.LocationQual,
		nullStr(in.PhotoURL), nullStr(in.ImageSig))
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`INSERT INTO reports
	 (id,event_id,source,reporter,report_time,segment_id,ref_segment_id,location_qual,photo_url,image_sig,created_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())`,
		in.ReportID, eventID, in.Source, in.Reporter, reportTime, in.SegmentID,
		nullStr(in.RefSegmentID), in.LocationQual, nullStr(in.PhotoURL), nullStr(in.ImageSig))
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`INSERT INTO event_versions
	 (event_id,version,change_kind,segment_id,ref_segment_id,location_qual,risk_radius_seg,
	  closed_segments,decision,decision_state,note,source,actor)
	 VALUES ($1,1,'INITIAL',$2,$3,$4,$5,$6,'CLOSED','FINAL','初始报告封闭',$7,$8)`,
		eventID, in.SegmentID, nullStr(in.RefSegmentID), in.LocationQual, radius,
		pq.StringArray(closure), in.Source, actor.UserID)
	if err != nil {
		return nil, err
	}
	if err := s.recordPropagation(tx, eventID, 1, closure, propEdges, 0); err != nil {
		return nil, err
	}
	if err := s.replaceSegmentStatus(tx, eventID, 1, closure, "CLOSED", in.Source, actor.UserID); err != nil {
		return nil, err
	}
	if err := s.createTasksForClosure(tx, eventID, 1, closure, in.Source, actor.UserID, nil); err != nil {
		return nil, err
	}
	if err := s.fanoutNotifications(tx, eventID, 1, "CLOSED", closure, "发现跑道异物，区段封闭", in.Source, actor.UserID); err != nil {
		return nil, err
	}
	s.audit(tx, "EVENT", eventID, "CREATE", 1, in.Source, actor.UserID,
		map[string]interface{}{"closure": closure, "radius": radius, "report_id": in.ReportID})

	return &ReportResult{
		ReportID: in.ReportID, EventID: eventID, Linked: false, NewVersion: 1,
		ClosedSegs: closure, Propagation: propEdges,
	}, tx.Commit()
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, x := range in {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
