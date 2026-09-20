package app

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// ---------- 清扫任务回执 ----------

type TaskActionResult struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	Version   int    `json:"version"`
	Current   bool   `json:"current"`
	Duplicate bool   `json:"duplicate,omitempty"` // 重复回执：已处于该状态
	Rejected  string `json:"rejected,omitempty"`  // 非空表示迟到/越版本回执被拒绝
}

func (s *Service) actOnTask(taskID, action, note, source string, actor Actor) (*TaskActionResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 全局加锁顺序：事件 -> 版本 -> 任务（与上报扩界路径一致），避免跨事务死锁。
	// 先无锁读出归属与事件。
	var eventID, contractor, status string
	var taskVer int
	err = tx.QueryRow(`SELECT event_id,contractor_id,version,status FROM cleanup_tasks WHERE id=$1`,
		taskID).Scan(&eventID, &contractor, &taskVer, &status)
	if err == sql.ErrNoRows {
		return nil, errf("NOT_FOUND", "任务不存在")
	}
	if err != nil {
		return nil, err
	}
	if actor.IsContractor() && contractor != actor.ContractorID {
		// 承包商只能操作分配给自己区段的任务
		return nil, errf("FORBIDDEN", "该任务不属于你所在承包商")
	}
	if _, err := lockEvent(tx, eventID); err != nil {
		return nil, err
	}
	head, err := s.currentVersion(tx, eventID, true)
	if err != nil {
		return nil, err
	}
	// 最后锁任务行本身
	var lockedStatus string
	var lockedVer int
	if err := tx.QueryRow(`SELECT version,status FROM cleanup_tasks WHERE id=$1 FOR UPDATE`,
		taskID).Scan(&lockedVer, &lockedStatus); err != nil {
		return nil, err
	}
	taskVer, status = lockedVer, lockedStatus

	res := &TaskActionResult{TaskID: taskID, Status: status, Version: taskVer, Current: taskVer == head.Version}

	if taskVer != head.Version || status == "CANCELLED_STALE" {
		res.Rejected = fmt.Sprintf("STALE_VERSION: 任务针对 v%d，当前版本 v%d", taskVer, head.Version)
		s.audit(tx, "TASK", taskID, "REJECT_"+action, taskVer, source, actor.UserID,
			map[string]interface{}{"reason": res.Rejected})
		return res, tx.Commit()
	}

	now := time.Now().UTC()
	duplicate := false
	switch action {
	case "ACCEPT":
		switch status {
		case "ASSIGNED":
			status = "ACCEPTED"
			_, err = tx.Exec(`UPDATE cleanup_tasks SET status='ACCEPTED',accepted_version=$2,accepted_at=$3 WHERE id=$1`,
				taskID, taskVer, now)
		case "ACCEPTED", "IN_PROGRESS", "COMPLETED":
			duplicate = true // 重复回执，状态不变
		}
	case "START":
		switch status {
		case "ACCEPTED":
			status = "IN_PROGRESS"
			_, err = tx.Exec(`UPDATE cleanup_tasks SET status='IN_PROGRESS',started_at=$2 WHERE id=$1`, taskID, now)
		case "IN_PROGRESS", "COMPLETED":
			duplicate = true
		default:
			return nil, errf("BAD_STATE", "任务尚未被接收，不能开始")
		}
	case "COMPLETE":
		switch status {
		case "IN_PROGRESS":
			status = "COMPLETED"
			_, err = tx.Exec(`UPDATE cleanup_tasks SET status='COMPLETED',completed_at=$2,completion_note=$3 WHERE id=$1`,
				taskID, now, nullStr(note))
		case "COMPLETED":
			duplicate = true
		default:
			return nil, errf("BAD_STATE", "任务未在进行中，不能完成（当前 %s）", status)
		}
	default:
		return nil, errf("BAD_REQUEST", "未知任务动作 %s", action)
	}
	if err != nil {
		return nil, err
	}
	s.audit(tx, "TASK", taskID, action, taskVer, source, actor.UserID,
		map[string]interface{}{"status": status, "duplicate": duplicate})

	res.Status = status
	res.Duplicate = duplicate
	return res, tx.Commit()
}

func (s *Service) AcceptTask(id, source string, a Actor) (*TaskActionResult, error) {
	return s.actOnTask(id, "ACCEPT", "", source, a)
}
func (s *Service) StartTask(id, source string, a Actor) (*TaskActionResult, error) {
	return s.actOnTask(id, "START", "", source, a)
}
func (s *Service) CompleteTask(id, note, source string, a Actor) (*TaskActionResult, error) {
	return s.actOnTask(id, "COMPLETE", note, source, a)
}

// ---------- 场务复查 ----------

type ReviewResult struct {
	ReviewID       string   `json:"review_id"`
	EventID        string   `json:"event_id"`
	ValidVersion   int      `json:"valid_version"`
	Result         string   `json:"result"`
	InvalidatedBy  *int     `json:"invalidated_by_version,omitempty"`
	Expanded       bool     `json:"expanded"`
	NewVersion     int      `json:"new_version,omitempty"`
	ClosedSegments []string `json:"closed_segments,omitempty"`
}

// SubmitFieldReview 记录复查。复查永远绑定提交时的当前版本；
// 一旦该事件之后扩大风险边界（新版本产生），复查立即失效（valid_version 落后）。
// 若复查发现封闭集外的残留（observe_segments 非空），视为定位证据扩大，立即开新版本。
func (s *Service) SubmitFieldReview(eventID string, in ReviewInput, observed []string, actor Actor) (*ReviewResult, error) {
	if in.Result != "CLEAR" && in.Result != "NOT_CLEAR" && in.Result != "RESIDUAL_FOUND" {
		return nil, errf("BAD_REQUEST", "result 必须为 CLEAR/NOT_CLEAR/RESIDUAL_FOUND")
	}
	if in.ReviewID == "" {
		in.ReviewID = newID("REV")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := lockEvent(tx, eventID); err != nil {
		return nil, err
	}
	head, err := s.currentVersion(tx, eventID, true)
	if err != nil {
		return nil, err
	}
	final, err := s.latestFinalVersion(tx, eventID)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(`INSERT INTO field_reviews
	 (id,event_id,valid_version,result,finding,reviewer,source,reviewed_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,now())`,
		in.ReviewID, eventID, final.Version, in.Result, nullStr(in.Finding), actor.UserID, in.Source)
	if err != nil {
		return nil, err
	}
	s.audit(tx, "REVIEW", in.ReviewID, in.Result, final.Version, in.Source, actor.UserID,
		map[string]interface{}{"finding": in.Finding, "observed": observed})

	res := &ReviewResult{ReviewID: in.ReviewID, EventID: eventID, ValidVersion: final.Version, Result: in.Result}

	// 残留出现在当前生效封闭集之外：扩大风险边界，旧复查（含本次）随之失效。
	var outside []string
	if len(observed) > 0 {
		for _, seg := range observed {
			if !contains(final.ClosedSegments, seg) {
				outside = append(outside, seg)
			}
		}
	}
	if in.Result == "RESIDUAL_FOUND" && len(outside) > 0 {
		newVer, union, err := s.expandFromEvidence(tx, eventID, head, outside,
			"SPANS_SEGMENTS", "FIELD_REVIEW:"+in.ReviewID, in.Source, actor)
		if err != nil {
			return nil, err
		}
		res.Expanded = true
		res.NewVersion = newVer
		res.ClosedSegments = union
	}
	return res, tx.Commit()
}

// expandFromEvidence 基于新定位证据（复查残留等）扩大封闭：作废 PENDING 提案、开 FINAL 新版本。
func (s *Service) expandFromEvidence(tx *sql.Tx, eventID string, head versionRow, newAnchors []string,
	qual, note, source string, actor Actor) (int, []string, error) {

	var airportID string
	if err := tx.QueryRow(`SELECT airport_id FROM fod_events WHERE id=$1`, eventID).Scan(&airportID); err != nil {
		return 0, nil, err
	}
	final, err := s.latestFinalVersion(tx, eventID)
	if err != nil {
		return 0, nil, err
	}
	g, err := s.loadAdjacency(tx, airportID)
	if err != nil {
		return 0, nil, err
	}
	radius := effectiveRadius(qual, final.RiskRadius)
	// 新证据锚点按当前半径传播；既有封闭集原样保留，不从旧锚点重新扩张。
	expanded, expandEdges := closureSet(g, dedupSorted(append([]string{}, newAnchors...)), radius)
	added, _ := diffSet(expanded, final.ClosedSegments)
	union := dedupSorted(append(append([]string{}, final.ClosedSegments...), added...))

	maxVer := head.Version
	newVer := maxVer + 1
	if head.State == "PENDING" {
		_, err = tx.Exec(`UPDATE event_versions SET decision_state='SUPERSEDED' WHERE id=$1`, head.ID)
		if err != nil {
			return 0, nil, err
		}
	}
	anchor := newAnchors[0]
	_, err = tx.Exec(`UPDATE fod_events SET segment_id=$2,location_qual=$3 WHERE id=$1`, eventID, anchor, qual)
	if err != nil {
		return 0, nil, err
	}
	var newRowID int64
	err = tx.QueryRow(`INSERT INTO event_versions
	 (event_id,version,change_kind,segment_id,ref_segment_id,location_qual,risk_radius_seg,
	  closed_segments,decision,decision_state,note,source,actor,created_at)
	 VALUES ($1,$2,'LOCATION_EXPAND',$3,NULL,$4,$5,$6,'CLOSED','FINAL',$7,$8,$9,now()) RETURNING id`,
		eventID, newVer, anchor, qual, radius, pq.StringArray(union), note, source, actor.UserID).Scan(&newRowID)
	if err != nil {
		return 0, nil, err
	}
	if head.State == "PENDING" {
		_, _ = tx.Exec(`UPDATE event_versions SET superseded_by=$2 WHERE id=$1`, head.ID, newRowID)
	}
	if err := s.recordPropagation(tx, eventID, newVer, union, expandEdges, final.Version); err != nil {
		return 0, nil, err
	}
	if err := s.replaceSegmentStatus(tx, eventID, newVer, union, "CLOSED", source, actor.UserID); err != nil {
		return 0, nil, err
	}
	if err := s.rolloverTasks(tx, eventID, final.Version, newVer, union, source, actor.UserID, added); err != nil {
		return 0, nil, err
	}
	if err := s.fanoutNotifications(tx, eventID, newVer, "CLOSED", union, "复查发现残留，风险边界扩大", source, actor.UserID); err != nil {
		return 0, nil, err
	}
	s.audit(tx, "EVENT", eventID, "EXPAND", newVer, source, actor.UserID,
		map[string]interface{}{"added": added, "anchors": newAnchors, "superseded": head.Version})
	return newVer, union, nil
}

// ---------- 缩小封闭 / 恢复运行：双确认 ----------

type ReopenResult struct {
	EventID       string   `json:"event_id"`
	TargetVersion int      `json:"target_version"`
	Decision      string   `json:"decision"`
	State         string   `json:"state"` // PENDING 直到两份独立确认齐备
	ClosedAfter   []string `json:"closed_segments_after"`
	ReopenedSegs  []string `json:"reopened_segments"`
	Reason        string   `json:"reason"`
}

// ProposeReopen 提出缩小封闭（segments 非空=剩余封闭集）或恢复运行（segments 为空）。
// 前置条件：当前 FINAL 版本所有任务已完成，且有绑定当前版本的 CLEAR 复查。
func (s *Service) ProposeReopen(eventID string, in ReopenInput, actor Actor) (*ReopenResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := lockEvent(tx, eventID); err != nil {
		return nil, err
	}
	head, err := s.currentVersion(tx, eventID, true)
	if err != nil {
		return nil, err
	}
	if head.State == "PENDING" {
		return nil, errf("ALREADY_PENDING", "v%d 缩小/恢复提案仍在等待双确认", head.Version)
	}
	final, err := s.latestFinalVersion(tx, eventID)
	if err != nil {
		return nil, err
	}

	// 所有当前封闭区段的清除任务必须已完成
	var open int
	err = tx.QueryRow(`SELECT count(*) FROM cleanup_tasks
	 WHERE event_id=$1 AND version=$2 AND status <> 'COMPLETED'`, eventID, final.Version).Scan(&open)
	if err != nil {
		return nil, err
	}
	if open > 0 {
		return nil, errf("TASKS_PENDING", "当前版本仍有 %d 个区段清除任务未完成", open)
	}
	// 必须有针对当前版本的 CLEAR 复查，且没有 NOT_CLEAR/RESIDUAL 的同版本复查
	var clear, bad int
	if _, err := tx.Exec(`SELECT 1`); err != nil { // 占位防止简单错误
	}
	err = tx.QueryRow(`SELECT count(*) FROM field_reviews
	 WHERE event_id=$1 AND valid_version=$2 AND result='CLEAR'`, eventID, final.Version).Scan(&clear)
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(`SELECT count(*) FROM field_reviews
	 WHERE event_id=$1 AND valid_version=$2 AND result <> 'CLEAR'`, eventID, final.Version).Scan(&bad)
	if err != nil {
		return nil, err
	}
	if clear == 0 || bad > 0 {
		return nil, errf("REVIEW_REQUIRED", "需要当前版本 v%d 的 CLEAR 场务复查（clear=%d, 非clear=%d）",
			final.Version, clear, bad)
	}

	fullReopen := len(in.Segments) == 0
	var remaining []string
	var reopened []string
	decision := "RESTRICTED"
	if fullReopen {
		decision = "REOPEN_PROPOSED"
		remaining = []string{}
		reopened = final.ClosedSegments
	} else {
		for _, seg := range in.Segments {
			if !contains(final.ClosedSegments, seg) {
				return nil, errf("BAD_REQUEST", "区段 %s 不在当前封闭集中", seg)
			}
		}
		remaining = dedupSorted(append([]string{}, in.Segments...))
		reopened, _ = diffSet(final.ClosedSegments, remaining)
		if len(reopened) == 0 {
			return nil, errf("NO_CHANGE", "拟保留封闭集与当前一致，没有可缩小的区段")
		}
	}

	newVer := head.Version + 1
	_, err = tx.Exec(`INSERT INTO event_versions
	 (event_id,version,change_kind,segment_id,ref_segment_id,location_qual,risk_radius_seg,
	  closed_segments,decision,decision_state,note,source,actor,created_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'PENDING',$10,$11,$12,now())`,
		eventID, newVer,
		map[bool]string{true: "SEGMENTS_SHRINK", false: "REOPEN"}[fullReopen],
		final.SegmentID, final.RefSegmentID, final.LocationQual, final.RiskRadius,
		pq.StringArray(remaining), decision, in.Reason, in.Source, actor.UserID)
	if err != nil {
		return nil, err
	}
	s.audit(tx, "EVENT", eventID, "PROPOSE_"+decision, newVer, in.Source, actor.UserID,
		map[string]interface{}{"remaining": remaining, "reopened": reopened, "proposer": actor.UserID})

	// 提案期间不通知塔台、不改 segment_status：塔台只能读到最终结论。
	return &ReopenResult{
		EventID: eventID, TargetVersion: newVer, Decision: decision, State: "PENDING",
		ClosedAfter: remaining, ReopenedSegs: reopened, Reason: in.Reason,
	}, tx.Commit()
}

type ApprovalResult struct {
	EventID       string `json:"event_id"`
	TargetVersion int    `json:"target_version"`
	Party         string `json:"party"`
	State         string `json:"state"`
	FieldApproved bool   `json:"field_approved"`
	OpsApproved   bool   `json:"ops_approved"`
	Duplicate     bool   `json:"duplicate,omitempty"`
	Rejected      string `json:"rejected,omitempty"`
	Finalized     bool   `json:"finalized"`
}

// Approve 提交场务/运行控制的独立确认。角色决定 party；同一 party 重复确认幂等；
// 迟到确认（目标版本已被扩大风险边界作废）拒绝；两份齐备且确认人互不相同方可生效。
func (s *Service) Approve(eventID string, targetVersion int, in ApprovalInput, actor Actor) (*ApprovalResult, error) {
	party := in.Party
	if party == "" {
		switch actor.Role {
		case "FIELD_CREW":
			party = "FIELD"
		case "OPS_CONTROLLER":
			party = "OPS"
		}
	}
	if party != "FIELD" && party != "OPS" {
		return nil, errf("BAD_REQUEST", "party 必须为 FIELD 或 OPS")
	}
	if party == "FIELD" && actor.Role != "FIELD_CREW" {
		return nil, errf("FORBIDDEN", "场务确认只能由 FIELD_CREW 提交")
	}
	if party == "OPS" && actor.Role != "OPS_CONTROLLER" {
		return nil, errf("FORBIDDEN", "运行控制确认只能由 OPS_CONTROLLER 提交")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := lockEvent(tx, eventID); err != nil {
		return nil, err
	}
	head, err := s.currentVersion(tx, eventID, true)
	if err != nil {
		return nil, err
	}
	if targetVersion == 0 {
		targetVersion = head.Version
	}
	res := &ApprovalResult{EventID: eventID, TargetVersion: targetVersion, Party: party}

	// 目标版本必须是当前版本，否则为迟到确认
	if head.Version != targetVersion {
		res.Rejected = fmt.Sprintf("LATE_VERSION: 目标 v%d 已不是当前版本 v%d", targetVersion, head.Version)
		s.audit(tx, "APPROVAL", eventID, "REJECT_APPROVAL", targetVersion, in.Source, actor.UserID,
			map[string]interface{}{"party": party, "reason": res.Rejected})
		return res, tx.Commit()
	}
	if head.State != "PENDING" {
		// 目标版本已经生效或作废：重复/迟到确认不改变任何结论
		res.State = head.State
		res.Duplicate = true
		if head.State == "SUPERSEDED" {
			res.Rejected = "SUPERSEDED: 目标提案已被新的定位证据作废"
		}
		s.audit(tx, "APPROVAL", eventID, "DUP_APPROVAL", targetVersion, in.Source, actor.UserID,
			map[string]interface{}{"party": party, "state": head.State})
		return res, tx.Commit()
	}

	// 幂等：该 party 已确认过则标记重复；UNIQUE 约束兜底并发。
	var existingApprover sql.NullString
	err = tx.QueryRow(`SELECT approver FROM reopen_approvals
	 WHERE event_id=$1 AND target_version=$2 AND party=$3`,
		eventID, targetVersion, party).Scan(&existingApprover)
	duplicateParty := err == nil && existingApprover.Valid
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	_, err = tx.Exec(`INSERT INTO reopen_approvals(event_id,target_version,party,approver,source,created_at)
	 VALUES ($1,$2,$3,$4,$5,now()) ON CONFLICT (event_id,target_version,party) DO NOTHING`,
		eventID, targetVersion, party, actor.UserID, in.Source)
	if err != nil {
		return nil, err
	}

	type appr struct {
		party    string
		approver string
	}
	var approvers []appr
	rows, err := tx.Query(`SELECT party,approver FROM reopen_approvals
	 WHERE event_id=$1 AND target_version=$2`, eventID, targetVersion)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a appr
		if err := rows.Scan(&a.party, &a.approver); err != nil {
			rows.Close()
			return nil, err
		}
		approvers = append(approvers, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	got := map[string]string{}
	for _, a := range approvers {
		got[a.party] = a.approver
	}
	res.FieldApproved = got["FIELD"] != ""
	res.OpsApproved = got["OPS"] != ""
	res.State = "PENDING"
	res.Duplicate = duplicateParty

	if res.FieldApproved && res.OpsApproved {
		if got["FIELD"] == got["OPS"] {
			return nil, errf("NOT_INDEPENDENT", "两份确认必须来自不同责任人")
		}
		if err := s.finalizeReopen(tx, eventID, head, in.Source, actor.UserID); err != nil {
			return nil, err
		}
		res.Finalized = true
		res.State = "FINAL"
	}
	s.audit(tx, "APPROVAL", eventID, "APPROVE", targetVersion, in.Source, actor.UserID,
		map[string]interface{}{"party": party, "finalized": res.Finalized})
	return res, tx.Commit()
}

// finalizeReopen 双确认齐备后让缩小/恢复提案生效。
func (s *Service) finalizeReopen(tx *sql.Tx, eventID string, head versionRow, source, actor string) error {
	final, err := s.latestFinalVersion(tx, eventID)
	if err != nil {
		return err
	}
	reopened, _ := diffSet(final.ClosedSegments, head.ClosedSegments)

	_, err = tx.Exec(`UPDATE event_versions SET decision_state='FINAL' WHERE id=$1`, head.ID)
	if err != nil {
		return err
	}
	// 事件版本上记录两位确认人与各自的确认时间（两份独立确认）
	field, ops, fieldAt, opsAt := approverNames(tx, eventID, head.Version)
	_, err = tx.Exec(`UPDATE event_versions
	 SET field_confirm_user=$2,field_confirm_at=$3,ops_confirm_user=$4,ops_confirm_at=$5 WHERE id=$1`,
		head.ID, field, fieldAt, ops, opsAt)
	if err != nil {
		return err
	}

	// 移出封闭集的区段恢复可用
	if err := s.reopenSegmentStatus(tx, eventID, head.Version, reopened, source, actor); err != nil {
		return err
	}
	// 注：仍封闭区段的当前版本清除任务均已完成（提案前置条件），不再重新分派；
	// 区段若在未来被新证据重新封闭，rolloverTasks 会把它作为"新增区段"派全新任务。
	// 旧版本未开始的任务作废（区段已恢复或任务已切到新版本）
	_, err = tx.Exec(`UPDATE cleanup_tasks SET status='CANCELLED_STALE'
	 WHERE event_id=$1 AND version<$2 AND status IN ('ASSIGNED','ACCEPTED')`, eventID, head.Version)
	if err != nil {
		return err
	}

	decision := "RESTRICTED"
	title := "封闭范围缩小"
	if head.Decision == "REOPEN_PROPOSED" || len(head.ClosedSegments) == 0 {
		decision = "OPEN"
		title = "跑道恢复运行"
		_, err = tx.Exec(`UPDATE event_versions SET decision='OPEN' WHERE id=$1`, head.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE fod_events SET closed=TRUE,closed_at=now() WHERE id=$1`, eventID)
		if err != nil {
			return err
		}
	}
	// 仍封闭区段的状态行更新到新版本
	if len(head.ClosedSegments) > 0 {
		if err := s.replaceSegmentStatus(tx, eventID, head.Version, head.ClosedSegments, decision, source, actor); err != nil {
			return err
		}
	}
	if err := s.fanoutNotifications(tx, eventID, head.Version, decision, head.ClosedSegments, title, source, actor); err != nil {
		return err
	}
	s.audit(tx, "EVENT", eventID, "FINALIZE_"+decision, head.Version, source, actor,
		nil)
	_ = reopened
	return nil
}

func approverNames(tx *sql.Tx, eventID string, version int) (field, ops interface{}, fieldAt, opsAt interface{}) {
	_ = tx.QueryRow(`SELECT approver,created_at FROM reopen_approvals
	 WHERE event_id=$1 AND target_version=$2 AND party='FIELD'`,
		eventID, version).Scan(&field, &fieldAt)
	_ = tx.QueryRow(`SELECT approver,created_at FROM reopen_approvals
	 WHERE event_id=$1 AND target_version=$2 AND party='OPS'`,
		eventID, version).Scan(&ops, &opsAt)
	return
}

// ---------- 通知回执 ----------

type ReceiptResult struct {
	NotificationID string `json:"notification_id"`
	Status         string `json:"status"`
	Duplicate      bool   `json:"duplicate,omitempty"`
	Rejected       string `json:"rejected,omitempty"`
}

var receiptOrder = map[string]int{"DELIVERED": 1, "READ": 2, "ACK": 3}
var receiptStatus = map[string]string{"DELIVERED": "DELIVERED", "READ": "READ", "ACK": "ACKED"}

// ReceiveReceipt 处理通知送达/阅读/确认回执。重复回执幂等留痕；
// 针对已被新版本取代的旧通知的迟到回执不能越过当前版本。
func (s *Service) ReceiveReceipt(notificationID string, in ReceiptInput, actor Actor) (*ReceiptResult, error) {
	if receiptOrder[in.Kind] == 0 {
		return nil, errf("BAD_REQUEST", "kind 必须为 DELIVERED/READ/ACK")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var eventID string
	var nVer int
	var status string
	err = tx.QueryRow(`SELECT event_id,version,status FROM notifications WHERE id=$1 FOR UPDATE`,
		notificationID).Scan(&eventID, &nVer, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf("NOT_FOUND", "通知不存在")
	}
	if err != nil {
		return nil, err
	}

	if _, err := lockEvent(tx, eventID); err != nil {
		return nil, err
	}
	head, err := s.currentVersion(tx, eventID, true)
	if err != nil {
		return nil, err
	}
	res := &ReceiptResult{NotificationID: notificationID, Status: status}

	// 该事件同渠道已有更新通知 → 本回执迟到，只记录不推进状态
	var newer int
	err = tx.QueryRow(`SELECT count(*) FROM notifications
	 WHERE event_id=$1 AND channel=(SELECT channel FROM notifications WHERE id=$2) AND version>$3`,
		eventID, notificationID, nVer).Scan(&newer)
	if err != nil {
		return nil, err
	}
	duplicate := false
	if newer > 0 || head.Version > nVer {
		duplicate = true
		res.Rejected = fmt.Sprintf("LATE: 通知针对 v%d，当前版本 v%d", nVer, head.Version)
	} else if receiptOrder[in.Kind] <= statusRank(status) {
		// 重复回执（同级别或状态已更靠后）：不回退、不重复推进
		duplicate = true
	}

	_, err = tx.Exec(`INSERT INTO notification_receipts(notification_id,receipt_kind,source,duplicate,received_at)
	 VALUES ($1,$2,$3,$4,now())`, notificationID, in.Kind, in.Source, duplicate)
	if err != nil {
		return nil, err
	}
	if duplicate {
		res.Duplicate = true
		s.audit(tx, "NOTIFICATION", notificationID, "RECEIPT_DUP", nVer, in.Source, actor.UserID,
			map[string]interface{}{"kind": in.Kind, "rejected": res.Rejected})
		return res, tx.Commit()
	}

	newStatus := receiptStatus[in.Kind]
	col := map[string]string{"DELIVERED": "delivered_at", "READ": "read_at", "ACK": "acked_at"}[in.Kind]
	_, err = tx.Exec(fmt.Sprintf(`UPDATE notifications SET status=$2,%s=now() WHERE id=$1`, col),
		notificationID, newStatus)
	if err != nil {
		return nil, err
	}
	s.audit(tx, "NOTIFICATION", notificationID, "RECEIPT_"+in.Kind, nVer, in.Source, actor.UserID, nil)
	res.Status = newStatus
	return res, tx.Commit()
}

func statusRank(status string) int {
	switch status {
	case "SENT":
		return 0
	case "DELIVERED":
		return 1
	case "READ":
		return 2
	case "ACKED":
		return 3
	case "FAILED":
		return 0
	}
	return 0
}

// ---------- 航班影响 ----------

// RecordFlightImpact 把一次航班处置（等待/延误/备降/取消/放行）锚定到具体异物事件，
// 是"从一笔航班延误反查整条处置链"的入口。ImpactID 为幂等键。
func (s *Service) RecordFlightImpact(flightID string, in FlightImpactInput, actor Actor) (string, error) {
	valid := map[string]bool{"HOLD": true, "DELAY": true, "DIVERT": true, "CANCEL": true, "RELEASED": true}
	if !valid[in.ImpactKind] {
		return "", errf("BAD_REQUEST", "impact_kind 非法")
	}
	if in.EventID == "" {
		return "", errf("BAD_REQUEST", "event_id 必填：航班影响必须锚定异物事件")
	}
	if in.ImpactID == "" {
		in.ImpactID = newID("IMP")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var fAirport, flightNo string
	err = tx.QueryRow(`SELECT airport_id,flight_no FROM flights WHERE id=$1`, flightID).
		Scan(&fAirport, &flightNo)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errf("NOT_FOUND", "航班不存在")
	}
	if err != nil {
		return "", err
	}
	var eAirport, segID string
	var closed bool
	err = tx.QueryRow(`SELECT airport_id,segment_id,closed FROM fod_events WHERE id=$1`, in.EventID).
		Scan(&eAirport, &segID, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errf("NOT_FOUND", "事件不存在")
	}
	if err != nil {
		return "", err
	}
	if fAirport != eAirport {
		return "", errf("BAD_REQUEST", "航班与事件不属于同一机场")
	}

	// 幂等：重复 ImpactID 返回原记录
	var exist string
	err = tx.QueryRow(`SELECT id FROM flight_impacts WHERE id=$1`, in.ImpactID).Scan(&exist)
	if err == nil {
		return in.ImpactID, tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	var notified bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM notifications WHERE event_id=$1 AND decision_state='FINAL')`,
		in.EventID).Scan(&notified)
	if err != nil {
		return "", err
	}

	_, err = tx.Exec(`INSERT INTO flight_impacts
	 (id,flight_id,event_id,impact_kind,segment_id,notified,source,actor,created_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())`,
		in.ImpactID, flightID, in.EventID, in.ImpactKind, segID, notified, in.Source, actor.UserID)
	if err != nil {
		return "", err
	}

	// 同步航班状态
	switch in.ImpactKind {
	case "DELAY":
		_, err = tx.Exec(`UPDATE flights SET status='DELAYED',delay_reason='FOD:'||$2 WHERE id=$1`,
			flightID, in.EventID)
	case "CANCEL":
		_, err = tx.Exec(`UPDATE flights SET status='CANCELLED',delay_reason='FOD:'||$2 WHERE id=$1`,
			flightID, in.EventID)
	case "DIVERT":
		_, err = tx.Exec(`UPDATE flights SET status='DIVERTED',delay_reason='FOD:'||$2 WHERE id=$1`,
			flightID, in.EventID)
	case "RELEASED":
		_, err = tx.Exec(`UPDATE flights SET status='DEPARTED',actual_dep=now(),delay_reason=NULL WHERE id=$1`,
			flightID)
	}
	if err != nil {
		return "", err
	}
	s.audit(tx, "FLIGHT_IMPACT", in.ImpactID, in.ImpactKind, nil, in.Source, actor.UserID,
		map[string]interface{}{"flight_id": flightID, "event_id": in.EventID, "flight_no": flightNo})
	return in.ImpactID, tx.Commit()
}

func lockEvent(tx *sql.Tx, eventID string) (string, error) {
	var id, airport string
	err := tx.QueryRow(`SELECT id,airport_id FROM fod_events WHERE id=$1 FOR UPDATE`, eventID).Scan(&id, &airport)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errf("NOT_FOUND", "事件不存在")
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func dedupSorted(in []string) []string {
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
