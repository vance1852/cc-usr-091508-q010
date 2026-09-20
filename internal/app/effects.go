package app

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// recordPropagation 把封闭集计算时实际走过的相邻边落库（多起点取首次到达的边）。
// carryFromVersion>0 时，先把该版本中仍位于封闭集内区段的传播依据带到新版本。
func (s *Service) recordPropagation(tx *sql.Tx, eventID string, version int,
	closure []string, used []PropEdge, carryFromVersion int) error {

	inClosure := map[string]bool{}
	for _, seg := range closure {
		inClosure[seg] = true
	}
	if carryFromVersion > 0 {
		_, err := tx.Exec(`INSERT INTO closure_propagation
		 (event_id,version,from_segment,to_segment,edge_kind,hops,created_at)
		 SELECT event_id,$2,from_segment,to_segment,edge_kind,hops,now()
		   FROM closure_propagation
		  WHERE event_id=$1 AND version=$3 AND to_segment = ANY($4)
		 ON CONFLICT (event_id,version,to_segment) DO NOTHING`,
			eventID, version, carryFromVersion, pq.Array(closure))
		if err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, e := range used {
		if !inClosure[e.ToSegment] || seen[e.ToSegment] {
			continue
		}
		seen[e.ToSegment] = true
		_, err := tx.Exec(`INSERT INTO closure_propagation
		 (event_id,version,from_segment,to_segment,edge_kind,hops,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,now())
		 ON CONFLICT (event_id,version,to_segment) DO NOTHING`,
			eventID, version, e.FromSegment, e.ToSegment, e.EdgeKind, e.Hops)
		if err != nil {
			return err
		}
	}
	return nil
}

// replaceSegmentStatus 把该事件的"非 AVAILABLE"生效状态推进到新版本：
// 旧版本 CLOSED/RESTRICTED 行标记 SUPERSEDED，新版本写 FINAL。AVAILABLE 行保留。
// 注意：PENDING 提案从不写 segment_status FINAL——塔台只能看到生效结论。
func (s *Service) replaceSegmentStatus(tx *sql.Tx, eventID string, version int, segments []string,
	state, source, actor string) error {

	_, err := tx.Exec(`UPDATE segment_status SET decision_state='SUPERSEDED'
	 WHERE event_id=$1 AND decision_state='FINAL' AND state <> 'AVAILABLE'`, eventID)
	if err != nil {
		return err
	}
	for _, seg := range segments {
		_, err := tx.Exec(`INSERT INTO segment_status
		 (event_id,segment_id,version,state,decision_state,source,actor,created_at)
		 VALUES ($1,$2,$3,$4,'FINAL',$5,$6,now())`,
			eventID, seg, version, state, source, actor)
		if err != nil {
			return err
		}
	}
	return nil
}

// segmentsToReopen：新版本中移出封闭集的区段恢复 AVAILABLE，旧 CLOSED 行同时失效。
func (s *Service) reopenSegmentStatus(tx *sql.Tx, eventID string, version int, reopened []string, source, actor string) error {
	for _, seg := range reopened {
		if _, err := tx.Exec(`UPDATE segment_status SET decision_state='SUPERSEDED'
		 WHERE event_id=$1 AND segment_id=$2 AND decision_state='FINAL' AND state<>'AVAILABLE'`,
			eventID, seg); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO segment_status
		 (event_id,segment_id,version,state,decision_state,source,actor,created_at)
		 VALUES ($1,$2,$3,'AVAILABLE','FINAL',$4,$5,now())`,
			eventID, seg, version, source, actor); err != nil {
			return err
		}
	}
	return nil
}

// contractorForSegment 选择区段的主责清扫承包商；交叉区段的备援承包商不主派。
func (s *Service) contractorForSegment(tx *sql.Tx, segmentID string) (string, error) {
	var c string
	err := tx.QueryRow(`SELECT contractor_id FROM segment_contractors
	 WHERE segment_id=$1 ORDER BY is_primary DESC, contractor_id LIMIT 1`, segmentID).Scan(&c)
	if err == sql.ErrNoRows {
		return "", errf("NO_CONTRACTOR", "区段 %s 未配置清扫承包商", segmentID)
	}
	return c, err
}

func (s *Service) createTasksForClosure(tx *sql.Tx, eventID string, version int, closure []string,
	source, actor string, onlyAdded map[string]bool) error {

	for _, seg := range closure {
		if onlyAdded != nil && !onlyAdded[seg] {
			continue
		}
		contractor, err := s.contractorForSegment(tx, seg)
		if err != nil {
			return err
		}
		tid := newID("TSK")
		_, err = tx.Exec(`INSERT INTO cleanup_tasks
		 (id,event_id,version,segment_id,contractor_id,status,source,assigned_by,created_at)
		 VALUES ($1,$2,$3,$4,$5,'ASSIGNED',$6,$7,now())
		 ON CONFLICT (event_id,version,segment_id) DO NOTHING`,
			tid, eventID, version, seg, contractor, source, actor)
		if err != nil {
			return err
		}
	}
	return nil
}

// rolloverTasks 在风险边界扩大时，为新封闭集的每个区段生成当前版本任务：
//   - 本次新纳入封闭集的区段（added，可能是历史上恢复后又重新封闭）一律分派
//     ASSIGNED 新任务：需要重新作业，不能沿用更早版本的完成进度；
//   - 持续封闭区段若已有实际作业（接收/进行中/完成），进度携带到新版本；
//   - 旧版本中仍 ASSIGNED 的任务标记 CANCELLED_STALE（其迟到回执会被版本校验拒绝）。
//
// 结果：每个封闭区段恰有一个"当前版本"任务；任何针对旧版本的回执都不能越过当前版本。
func (s *Service) rolloverTasks(tx *sql.Tx, eventID string, oldVer, newVer int, newClosure []string,
	source, actor string, added []string) error {

	addedSet := map[string]bool{}
	for _, a := range added {
		addedSet[a] = true
	}
	for _, seg := range newClosure {
		contractor, err := s.contractorForSegment(tx, seg)
		if err != nil {
			return err
		}
		tid := newID("TSK")

		// 本次新纳入封闭的区段：全新分派，不参考任何历史任务
		if addedSet[seg] {
			_, err = tx.Exec(`INSERT INTO cleanup_tasks
			 (id,event_id,version,segment_id,contractor_id,status,source,assigned_by,created_at)
			 VALUES ($1,$2,$3,$4,$5,'ASSIGNED',$6,$7,now())`,
				tid, eventID, newVer, seg, contractor, source, actor)
			if err != nil {
				return err
			}
			continue
		}

		// 持续封闭区段：携带同区段最近旧版本任务的实际作业进度
		var oldStatus string
		var oldAcceptedVer sql.NullInt64
		var oldAcceptedAt, oldStartedAt, oldCompletedAt sql.NullTime
		var oldNote sql.NullString
		err = tx.QueryRow(`SELECT status,accepted_version,accepted_at,started_at,completed_at,completion_note
		 FROM cleanup_tasks WHERE event_id=$1 AND segment_id=$2 AND version<$3
		 ORDER BY version DESC LIMIT 1`, eventID, seg, newVer).
			Scan(&oldStatus, &oldAcceptedVer, &oldAcceptedAt, &oldStartedAt, &oldCompletedAt, &oldNote)
		if err == sql.ErrNoRows {
			_, err = tx.Exec(`INSERT INTO cleanup_tasks
			 (id,event_id,version,segment_id,contractor_id,status,source,assigned_by,created_at)
			 VALUES ($1,$2,$3,$4,$5,'ASSIGNED',$6,$7,now())`,
				tid, eventID, newVer, seg, contractor, source, actor)
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		newStatus := oldStatus
		if oldStatus == "CANCELLED_STALE" {
			newStatus = "ASSIGNED"
		}
		_, err = tx.Exec(`INSERT INTO cleanup_tasks
		 (id,event_id,version,segment_id,contractor_id,status,accepted_version,accepted_at,
		  started_at,completed_at,completion_note,source,assigned_by,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,now())`,
			tid, eventID, newVer, seg, contractor, newStatus,
			sql.NullInt64{Int64: int64(newVer), Valid: newStatus != "ASSIGNED"},
			oldAcceptedAt, oldStartedAt, oldCompletedAt,
			sql.NullString{String: "沿用 v" + itoa(oldVer) + " 作业进度", Valid: newStatus != "ASSIGNED"},
			source, actor)
		if err != nil {
			return err
		}
	}
	// 旧版本未开始任务全部作废（包括已携带进度区段的旧 ASSIGNED 行）
	_, err := tx.Exec(`UPDATE cleanup_tasks SET status='CANCELLED_STALE'
	 WHERE event_id=$1 AND version<$2 AND status='ASSIGNED'`, eventID, newVer)
	return err
}

// fanoutNotifications 向塔台与放行席发布最终运行结论；通知正文只含运行结论与区段，绝不含旅客信息。
func (s *Service) fanoutNotifications(tx *sql.Tx, eventID string, version int, decision string,
	segments []string, title, source, actor string) error {

	channels := []struct{ ch, recipient string }{
		{"TOWER", "塔台值班"},
		{"DEPARTURE_CONTROL", "航班放行席"},
		{"DISPATCH", "场务调度"},
	}
	runwayParts := map[string]bool{}
	rows, err := tx.Query(`SELECT id, runway, name FROM runway_segments WHERE id = ANY($1)`, pq.Array(segments))
	if err != nil {
		return err
	}
	nameOf := map[string]string{}
	for rows.Next() {
		var id, rw, name string
		if err := rows.Scan(&id, &rw, &name); err != nil {
			rows.Close()
			return err
		}
		nameOf[id] = name
		runwayParts[rw] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rwys := make([]string, 0, len(runwayParts))
	for r := range runwayParts {
		rwys = append(rwys, r)
	}
	segs := make([]string, len(segments))
	for i, seg := range segments {
		segs[i] = seg + "(" + nameOf[seg] + ")"
	}
	body := fmt.Sprintf("运行结论 %s v%d：跑道 %s；区段 %s。",
		decisionText(decision), version, strings.Join(rwys, "/"), strings.Join(segs, "、"))

	for _, c := range channels {
		nid := newID("NTF")
		_, err := tx.Exec(`INSERT INTO notifications
		 (id,event_id,version,channel,recipient,subject,body,decision,decision_state,status,source,created_by,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'FINAL','SENT',$9,$10,now())`,
			nid, eventID, version, c.ch, c.recipient, title, body, decision, source, actor)
		if err != nil {
			return err
		}
	}
	return nil
}

func decisionText(d string) string {
	switch d {
	case "CLOSED":
		return "封闭"
	case "RESTRICTED":
		return "限制使用"
	case "OPEN":
		return "恢复运行"
	}
	return d
}
