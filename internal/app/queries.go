package app

import (
	"database/sql"
	"time"
)

// ---------- 事件详情 ----------

func (s *Service) GetEvent(eventID string, actor Actor) (*EventDetail, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var d EventDetail
	var ref sql.NullString
	var photo, sig sql.NullString
	var closedAt sql.NullTime
	err = tx.QueryRow(`SELECT id,airport_id,segment_id,ref_segment_id,location_qual,photo_url,image_sig,
	 closed,closed_at,created_at FROM fod_events WHERE id=$1`, eventID).
		Scan(&d.ID, &d.AirportID, &d.SegmentID, &ref, &d.LocationQual, &photo, &sig,
			&d.Closed, &closedAt, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, errf("NOT_FOUND", "事件不存在")
	}
	if err != nil {
		return nil, err
	}
	d.RefSegmentID = ref.String
	d.PhotoURL = photo.String
	d.ImageSig = sig.String
	if closedAt.Valid {
		t := closedAt.Time
		d.ClosedAt = &t
	}

	rows, err := tx.Query(`SELECT version,change_kind,segment_id,ref_segment_id,location_qual,
	 risk_radius_seg,closed_segments,decision,decision_state,field_confirm_at,ops_confirm_at,
	 COALESCE(note,''),source,actor,created_at
	 FROM event_versions WHERE event_id=$1 ORDER BY version`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v VersionView
		var ref sql.NullString
		var fc, oc sql.NullTime
		if err := rows.Scan(&v.Version, &v.ChangeKind, &v.SegmentID, &ref, &v.LocationQual,
			&v.RiskRadiusSeg, pqArray(&v.ClosedSegments), &v.Decision, &v.DecisionState,
			&fc, &oc, &v.Note, &v.Source, &v.Actor, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.RefSegmentID = ref.String
		if fc.Valid {
			v.FieldConfirm = &fc.Time
		}
		if oc.Valid {
			v.OpsConfirm = &oc.Time
		}
		d.Versions = append(d.Versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(d.Versions) > 0 {
		d.CurrentVersion = d.Versions[len(d.Versions)-1].Version
	}
	final, err := s.latestFinalVersion(tx, eventID)
	if err == nil {
		d.CurrentClosed = final.ClosedSegments
	}
	return &d, tx.Commit()
}

func (s *Service) ListOpenEvents(airportID string) ([]map[string]interface{}, error) {
	rows, err := s.db.Query(`SELECT e.id,e.segment_id,COALESCE(e.ref_segment_id,''),e.location_qual,e.created_at,
	 (SELECT version FROM event_versions v WHERE v.event_id=e.id ORDER BY version DESC LIMIT 1),
	 (SELECT decision_state FROM event_versions v WHERE v.event_id=e.id ORDER BY version DESC LIMIT 1),
	 COALESCE((SELECT closed_segments FROM event_versions v WHERE v.event_id=e.id AND decision_state='FINAL'
	  ORDER BY version DESC LIMIT 1),'{}')
	 FROM fod_events e WHERE e.airport_id=$1 AND e.closed=FALSE ORDER BY e.created_at`, airportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var id, seg, ref, qual string
		var createdAt time.Time
		var ver int
		var state string
		var closed []string
		if err := rows.Scan(&id, &seg, &ref, &qual, &createdAt, &ver, &state, pqArray(&closed)); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{
			"event_id": id, "segment_id": seg, "ref_segment_id": ref,
			"location_qual": qual, "current_version": ver, "head_state": state,
			"effective_closed_segments": closed, "created_at": createdAt,
		})
	}
	return out, rows.Err()
}

// ---------- 塔台：最终运行结论 ----------

// TowerOperationalStatus 汇总全场每个区段当前生效结论。只取 FINAL 行，
// PENDING 提案绝不出现在塔台视图。同区段被多事件影响时取最严格状态，
// 并给出导致封闭/限制的事件列表作为来源。
func (s *Service) TowerOperationalStatus(airportID string) ([]SegmentOperationalStatus, error) {
	rows, err := s.db.Query(`
	 SELECT rs.id, rs.runway, rs.name,
	        COALESCE(l.state,'AVAILABLE'),
	        COALESCE(l.reasons,'{}'),
	        l.at
	   FROM runway_segments rs
	   LEFT JOIN LATERAL (
	     SELECT state, array_agg(event_id) AS reasons, max(created_at) AS at FROM (
	       SELECT DISTINCT ON (event_id, segment_id)
	              segment_id, state, event_id, created_at
	         FROM segment_status
	        WHERE decision_state='FINAL' AND segment_id=rs.id
	        ORDER BY event_id,segment_id,version DESC,id DESC
	     ) per
	     WHERE per.state <> 'AVAILABLE'
	     GROUP BY per.state
	     ORDER BY CASE per.state WHEN 'CLOSED' THEN 1 WHEN 'RESTRICTED' THEN 2 END
	     LIMIT 1
	   ) l ON TRUE
	  WHERE rs.airport_id=$1
	  ORDER BY rs.runway, rs.seq_no`, airportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SegmentOperationalStatus
	for rows.Next() {
		var st SegmentOperationalStatus
		var reasons []string
		var at sql.NullTime
		if err := rows.Scan(&st.SegmentID, &st.Runway, &st.Name, &st.State,
			pqArray(&reasons), &at); err != nil {
			return nil, err
		}
		for _, r := range reasons {
			st.Reasons = append(st.Reasons, "FOD:"+r)
		}
		if at.Valid {
			st.UpdatedAt = at.Time
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---------- 承包商：只能看到本承包商被分派区段的任务，无旅客信息 ----------

func (s *Service) ListTasks(actor Actor, onlyCurrent bool) ([]TaskView, error) {
	q := `SELECT t.id,t.event_id,t.version,t.segment_id,COALESCE(rs.name,t.segment_id),
	                t.status,t.created_at,
	                (SELECT max(version) FROM event_versions v WHERE v.event_id=t.event_id)
	          FROM cleanup_tasks t
	          LEFT JOIN runway_segments rs ON rs.id=t.segment_id`
	args := []interface{}{}
	if actor.IsContractor() {
		q += ` WHERE t.contractor_id=$1`
		args = append(args, actor.ContractorID)
	}
	if onlyCurrent {
		if actor.IsContractor() {
			q += ` AND t.version=(SELECT max(version) FROM event_versions v WHERE v.event_id=t.event_id)`
		} else {
			q += ` WHERE t.version=(SELECT max(version) FROM event_versions v WHERE v.event_id=t.event_id)`
		}
	}
	q += ` ORDER BY t.created_at`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func scanTasks(rows *sql.Rows) ([]TaskView, error) {
	var out []TaskView
	for rows.Next() {
		var t TaskView
		var headVer int
		if err := rows.Scan(&t.ID, &t.EventID, &t.Version, &t.SegmentID, &t.SegmentName,
			&t.Status, &t.CreatedAt, &headVer); err != nil {
			return nil, err
		}
		t.Stale = t.Version < headVer
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- 航班延误反查时间线 ----------

type TimelineEntry struct {
	At      time.Time              `json:"at"`
	Kind    string                 `json:"kind"`
	Source  string                 `json:"source"`
	Actor   string                 `json:"actor"`
	Version *int                   `json:"version,omitempty"`
	Title   string                 `json:"title"`
	Detail  map[string]interface{} `json:"detail,omitempty"`
}

type FlightTrace struct {
	FlightID string                   `json:"flight_id"`
	FlightNo string                   `json:"flight_no"`
	Status   string                   `json:"status"`
	Impacts  []map[string]interface{} `json:"impacts"`
	Timeline []TimelineEntry          `json:"timeline"`
}

// TraceFlight 从一笔航班出发，沿 flight_impacts -> event -> 版本/传播/任务/复查/确认/通知，
// 汇总完整处置链，并标注每条通知是否送达。
func (s *Service) TraceFlight(flightID string) (*FlightTrace, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var tr FlightTrace
	var delayReason sql.NullString
	err = tx.QueryRow(`SELECT id,flight_no,COALESCE(status,''),COALESCE(delay_reason,'')
	 FROM flights WHERE id=$1`, flightID).Scan(&tr.FlightID, &tr.FlightNo, &tr.Status, &delayReason)
	if err == sql.ErrNoRows {
		return nil, errf("NOT_FOUND", "航班不存在")
	}
	if err != nil {
		return nil, err
	}

	impRows, err := tx.Query(`SELECT fi.id,fi.event_id,fi.impact_kind,COALESCE(fi.segment_id,''),
	 fi.notified,fi.source,fi.actor,fi.created_at
	 FROM flight_impacts fi WHERE fi.flight_id=$1 ORDER BY fi.created_at`, flightID)
	if err != nil {
		return nil, err
	}
	type imp struct {
		id, eventID, kind, seg, source, actor string
		notified                              bool
		at                                    time.Time
	}
	var imps []imp
	eventSet := map[string]bool{}
	for impRows.Next() {
		var x imp
		if err := impRows.Scan(&x.id, &x.eventID, &x.kind, &x.seg, &x.notified, &x.source, &x.actor, &x.at); err != nil {
			impRows.Close()
			return nil, err
		}
		imps = append(imps, x)
		eventSet[x.eventID] = true
		tr.Impacts = append(tr.Impacts, map[string]interface{}{
			"impact_id": x.id, "event_id": x.eventID, "impact_kind": x.kind,
			"segment_id": x.seg, "notified": x.notified, "source": x.source,
			"actor": x.actor, "at": x.at,
		})
	}
	impRows.Close()
	if err := impRows.Err(); err != nil {
		return nil, err
	}

	for eventID := range eventSet {
		if err := s.appendEventTrace(tx, eventID, &tr.Timeline); err != nil {
			return nil, err
		}
	}
	// 时间线按时间排序，版本内按类别稳定
	sortTimeline(tr.Timeline)
	return &tr, tx.Commit()
}

func (s *Service) appendEventTrace(tx *sql.Tx, eventID string, tl *[]TimelineEntry) error {
	add := func(at time.Time, kind, source, actor, title string, ver *int, detail map[string]interface{}) {
		*tl = append(*tl, TimelineEntry{At: at, Kind: kind, Source: source, Actor: actor,
			Version: ver, Title: title, Detail: detail})
	}

	// 报告
	rows, err := tx.Query(`SELECT id,source,reporter,report_time,COALESCE(link_basis,''),
	 COALESCE(linked_event_id,'') FROM reports WHERE event_id=$1 ORDER BY report_time`, eventID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, source, reporter, basis, linked string
		var at time.Time
		if err := rows.Scan(&id, &source, &reporter, &at, &basis, &linked); err != nil {
			rows.Close()
			return err
		}
		d := map[string]interface{}{"report_id": id, "reporter": reporter}
		if linked != "" {
			d["linked"] = linked
			d["link_basis"] = basis
		}
		add(at, "REPORT", source, reporter, "异物报告"+suffix(basis), nil, d)
	}
	rows.Close()

	// 版本（含封闭集、双确认）
	vrows, err := tx.Query(`SELECT version,change_kind,decision,decision_state,closed_segments,
	 COALESCE(note,''),source,actor,created_at,
	 COALESCE(field_confirm_user,''),COALESCE(ops_confirm_user,'')
	 FROM event_versions WHERE event_id=$1 ORDER BY version`, eventID)
	if err != nil {
		return err
	}
	for vrows.Next() {
		var ver int
		var change, decision, state, note, source, actor, fc, oc string
		var segs []string
		var at time.Time
		if err := vrows.Scan(&ver, &change, &decision, &state, pqArray(&segs), &note, &source, &actor, &at, &fc, &oc); err != nil {
			vrows.Close()
			return err
		}
		v := ver
		d := map[string]interface{}{"closed_segments": segs, "change": change}
		if note != "" {
			d["note"] = note
		}
		if fc != "" {
			d["field_confirm"] = fc
		}
		if oc != "" {
			d["ops_confirm"] = oc
		}
		add(at, "VERSION:"+change+":"+state, source, actor,
			"v"+itoa(ver)+" "+change+" => "+decision+"("+state+")", &v, d)
	}
	vrows.Close()
	if err := vrows.Err(); err != nil {
		return err
	}

	// 封闭传播
	prows, err := tx.Query(`SELECT version,from_segment,to_segment,edge_kind,hops,created_at
	 FROM closure_propagation WHERE event_id=$1 ORDER BY version,hops,to_segment`, eventID)
	if err != nil {
		return err
	}
	for prows.Next() {
		var ver, hops int
		var from, to, kind string
		var at time.Time
		if err := prows.Scan(&ver, &from, &to, &kind, &hops, &at); err != nil {
			prows.Close()
			return err
		}
		v := ver
		add(at, "PROPAGATION", "SYSTEM", "system", from+" -> "+to+" ("+kind+","+itoa(hops)+"跳)", &v,
			map[string]interface{}{"from": from, "to": to, "edge": kind, "hops": hops})
	}
	prows.Close()

	// 清除任务
	trows, err := tx.Query(`SELECT id,version,segment_id,contractor_id,status,source,assigned_by,created_at,
	 COALESCE(completed_at,'1970-01-01 00:00:00 UTC')
	 FROM cleanup_tasks WHERE event_id=$1 ORDER BY created_at`, eventID)
	if err != nil {
		return err
	}
	for trows.Next() {
		var id, seg, contractor, status, source, assignedBy string
		var ver int
		var at, comp time.Time
		if err := trows.Scan(&id, &ver, &seg, &contractor, &status, &source, &assignedBy, &at, &comp); err != nil {
			trows.Close()
			return err
		}
		v := ver
		d := map[string]interface{}{"task_id": id, "segment": seg, "contractor": contractor,
			"assigned_by": assignedBy}
		if status == "COMPLETED" {
			d["completed_at"] = comp
			add(comp, "TASK_COMPLETE", source, contractor, seg+" 清除完成", &v, d)
		} else {
			add(at, "TASK:"+status, source, assignedBy, seg+" 分派清除任务("+status+")", &v, d)
		}
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return err
	}

	// 复查（标注提交后是否已被扩边界失效）
	rrows, err := tx.Query(`SELECT id,valid_version,result,COALESCE(finding,''),reviewer,source,reviewed_at,
	 (SELECT max(version) FROM event_versions WHERE event_id=$1)
	 FROM field_reviews WHERE event_id=$1 ORDER BY reviewed_at`, eventID)
	if err != nil {
		return err
	}
	for rrows.Next() {
		var id, result, finding, reviewer, source string
		var validVer, headVer int
		var at time.Time
		if err := rrows.Scan(&id, &validVer, &result, &finding, &reviewer, &source, &at, &headVer); err != nil {
			rrows.Close()
			return err
		}
		valid := validVer >= headVer
		d := map[string]interface{}{"review_id": id, "valid_version": validVer, "still_valid": valid}
		if finding != "" {
			d["finding"] = finding
		}
		title := "场务复查 " + result
		if !valid {
			title += "（已被 v" + itoa(validVer+1) + " 扩边界失效）"
		}
		add(at, "REVIEW", source, reviewer, title, &validVer, d)
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return err
	}

	// 通知及送达情况
	nrows, err := tx.Query(`SELECT id,version,channel,recipient,decision,decision_state,status,
	 COALESCE(delivered_at,'1970-01-01 00:00:00 UTC'),
	 COALESCE(acked_at,'1970-01-01 00:00:00 UTC'),source,created_by,created_at
	 FROM notifications WHERE event_id=$1 ORDER BY created_at`, eventID)
	if err != nil {
		return err
	}
	for nrows.Next() {
		var id, ch, recipient, decision, dstate, status, source, by string
		var ver int
		var del, ack, at time.Time
		if err := nrows.Scan(&id, &ver, &ch, &recipient, &decision, &dstate, &status, &del, &ack, &source, &by, &at); err != nil {
			nrows.Close()
			return err
		}
		v := ver
		delivered := status != "SENT" && status != "FAILED"
		add(at, "NOTIFY", source, by, "通知 "+ch+"["+status+"] "+decision, &v,
			map[string]interface{}{"notification_id": id, "channel": ch, "recipient": recipient,
				"delivered": delivered, "acked": status == "ACKED", "decision_state": dstate})
	}
	nrows.Close()
	return nrows.Err()
}

func suffix(basis string) string {
	if basis == "" {
		return ""
	}
	return "（关联依据 " + basis + "）"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func sortTimeline(t []TimelineEntry) {
	for i := 1; i < len(t); i++ {
		for j := i; j > 0 && t[j-1].At.After(t[j].At); j-- {
			t[j-1], t[j] = t[j], t[j-1]
		}
	}
}
