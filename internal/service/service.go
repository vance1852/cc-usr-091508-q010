// Package service 编排业务事务:报告并案、封闭传播、清除分派、复查、双确认。
// 每个公开方法把一次状态迁移放进单个数据库事务,并用 SELECT ... FOR UPDATE
// 锁住事件行,保证并发复查/并发回执/边界扩大彼此串行化。
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"fodsys/internal/closure"
	"fodsys/internal/match"
	"fodsys/internal/model"
	"fodsys/internal/store"
)

type Service struct {
	st *store.Store
}

func New(st *store.Store) *Service { return &Service{st: st} }

// Actor 记录一次操作的责任人,写入审计。
type Actor struct {
	Role   string
	Name   string
	Source string // 来源系统/席位,如 'ops-console'、'tower-radio'
}

func (a Actor) source() string {
	if a.Source == "" {
		return a.Role
	}
	return a.Source
}

// loadGraph 在事务内加载邻接拓扑。
func (s *Service) loadGraph(ctx context.Context, q store.Querier) (*closure.Graph, []model.Segment, error) {
	segs, edges, err := s.st.LoadGraph(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	cs := make([]closure.Segment, 0, len(segs))
	for _, sg := range segs {
		cs = append(cs, closure.Segment{ID: sg.ID, Kind: sg.Kind})
	}
	return closure.NewGraph(cs, edges), segs, nil
}

// withTx 包装事务执行。
func (s *Service) withTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.st.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- 报告进入:并案 + 封闭计算 + 通知 ----

type ReportInput struct {
	SegmentID      string `json:"segment_id" binding:"required"`
	ImageSignature string `json:"image_signature"`
	Reporter       string `json:"reporter" binding:"required"`
	Source         string `json:"source" binding:"required"`
	RiskLevel      string `json:"risk_level"` // 仅新建事件时生效
	LocationDesc   string `json:"location_desc"`
}

type ReportResult struct {
	Report          model.Report  `json:"report"`
	Event           model.Event   `json:"event"`
	Closure         model.Closure `json:"closure"`
	MatchedExisting bool          `json:"matched_existing"`
	Expanded        bool          `json:"expanded"` // 本次报告是否扩大了风险边界
}

// SubmitReport 接收异物报告:先按位置+影像特征并案,再重算封闭边界。
// 边界扩大时旧复查立即失效,并向塔台/放行席推送新的区段结论。
func (s *Service) SubmitReport(ctx context.Context, in ReportInput, actor Actor) (*ReportResult, error) {
	if in.RiskLevel == "" {
		in.RiskLevel = "medium"
	}
	var res ReportResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		g, _, err := s.loadGraph(ctx, tx)
		if err != nil {
			return err
		}
		if _, ok := g.Segments[in.SegmentID]; !ok {
			return fmt.Errorf("%w: 未知区段 %s", store.ErrPrecondition, in.SegmentID)
		}

		// 1. 并案:在未闭环事件中找位置相近且影像相似者
		events, err := s.st.OpenEventCandidates(ctx, tx)
		if err != nil {
			return err
		}
		cands := make([]match.Candidate, 0, len(events))
		for _, e := range events {
			cands = append(cands, match.Candidate{EventID: e.ID, SeedSegments: e.SeedSegments, ImageSignature: e.ImageSignature})
		}
		isAdjacent := func(eventSegs []string, seg string) bool {
			for _, es := range eventSegs {
				for _, nb := range g.Adj[es] {
					if nb == seg {
						return true
					}
				}
			}
			return false
		}
		matchedID, matched := match.Best(in.SegmentID, in.ImageSignature, cands, isAdjacent)

		var ev model.Event
		if matched {
			// 锁定已有事件,合并证据
			ev, err = s.st.GetEventForUpdate(ctx, tx, matchedID)
			if err != nil {
				return err
			}
		} else {
			// 新建事件
			ev = model.Event{
				Status:         model.EventOpen,
				SeedSegments:   []string{in.SegmentID},
				RiskLevel:      in.RiskLevel,
				ImageSignature: in.ImageSignature,
				LocationDesc:   in.LocationDesc,
				CreatedBy:      actor.Name,
			}
			if err := s.st.InsertEvent(ctx, tx, &ev); err != nil {
				return err
			}
			if err := s.st.Audit(ctx, tx, "event", ev.ID, "created", actor.Name, actor.source(),
				map[string]any{"segment": in.SegmentID, "risk": in.RiskLevel}); err != nil {
				return err
			}
		}

		// 2. 登记报告
		rep := model.Report{
			EventID: ev.ID, SegmentID: in.SegmentID, ImageSignature: in.ImageSignature,
			Reporter: in.Reporter, Source: in.Source, MatchedExisting: matched,
		}
		if err := s.st.InsertReport(ctx, tx, &rep); err != nil {
			return err
		}
		if err := s.st.Audit(ctx, tx, "event", ev.ID, "report_filed", actor.Name, actor.source(),
			map[string]any{"report_id": rep.ID, "segment": in.SegmentID, "matched": matched}); err != nil {
			return err
		}

		// 3. 证据并入种子(只增不减),重算封闭
		seedSet := map[string]bool{}
		for _, s := range ev.SeedSegments {
			seedSet[s] = true
		}
		if !seedSet[in.SegmentID] {
			ev.SeedSegments = append(ev.SeedSegments, in.SegmentID)
			sort.Strings(ev.SeedSegments)
		}
		newSegments := g.Compute(ev.SeedSegments, ev.RiskLevel)

		var cur model.Closure
		cur, err = s.st.CurrentClosure(ctx, tx, ev.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}

		res.Report = rep
		res.MatchedExisting = matched

		switch {
		case errors.Is(err, store.ErrNotFound):
			// 首个封闭版本
			cl := model.Closure{EventID: ev.ID, Version: 1, Segments: newSegments, Kind: "initial",
				Status: model.ClosureActive, CreatedBy: actor.Name}
			if err := s.st.InsertClosure(ctx, tx, &cl); err != nil {
				return err
			}
			ev.ClosureVersion = 1
			if err := s.st.UpdateEvent(ctx, tx, &ev); err != nil {
				return err
			}
			if err := s.notifyClosure(ctx, tx, &ev, &cl, "closed"); err != nil {
				return err
			}
			if err := s.st.Audit(ctx, tx, "closure", cl.ID, "closure_issued", actor.Name, actor.source(),
				map[string]any{"event_id": ev.ID, "version": 1, "segments": newSegments}); err != nil {
				return err
			}
			res.Event, res.Closure = ev, cl

		case closure.IsSuperset(cur.Segments, newSegments):
			// 边界扩大:新版本 + 旧复查立即失效 + 事件回退到清除阶段
			if err := s.st.SupersedeClosure(ctx, tx, cur.ID); err != nil {
				return err
			}
			cl := model.Closure{EventID: ev.ID, Version: cur.Version + 1, Segments: newSegments, Kind: "expanded",
				Status: model.ClosureActive, CreatedBy: actor.Name}
			if err := s.st.InsertClosure(ctx, tx, &cl); err != nil {
				return err
			}
			n, err := s.st.SupersedeOpenReviews(ctx, tx, ev.ID)
			if err != nil {
				return err
			}
			ev.ClosureVersion = cl.Version
			if ev.Status != model.EventClosed {
				ev.Status = model.EventClearing // 新区域需要清除与复查
			}
			if err := s.st.UpdateEvent(ctx, tx, &ev); err != nil {
				return err
			}
			if err := s.notifyClosure(ctx, tx, &ev, &cl, "closed"); err != nil {
				return err
			}
			if err := s.st.Audit(ctx, tx, "closure", cl.ID, "closure_expanded", actor.Name, actor.source(),
				map[string]any{"event_id": ev.ID, "version": cl.Version, "segments": newSegments,
					"reviews_superseded": n}); err != nil {
				return err
			}
			res.Event, res.Closure, res.Expanded = ev, cl, true

		default:
			// 边界不变:仅落报告与种子
			if err := s.st.UpdateEvent(ctx, tx, &ev); err != nil {
				return err
			}
			res.Event, res.Closure = ev, cur
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// notifyClosure 向塔台与航班放行席推送区段结论(塔台只读最终结论)。
func (s *Service) notifyClosure(ctx context.Context, tx pgx.Tx, ev *model.Event, cl *model.Closure, availability string) error {
	payload := map[string]any{
		"event_id": ev.ID, "closure_id": cl.ID, "closure_version": cl.Version,
		"segments": cl.Segments, "availability": availability,
	}
	for _, ch := range []string{"tower", "flight_dispatch"} {
		n := model.Notification{EventID: ev.ID, Channel: ch,
			Subject: fmt.Sprintf("closure v%d %s", cl.Version, cl.Kind), Payload: payload}
		if err := s.st.InsertNotification(ctx, tx, &n); err != nil {
			return err
		}
	}
	return nil
}

// ---- 清除任务 ----

// AssignTask 把封闭集合中尚未覆盖的区段分派给承包商。
// 承包商通知只含区段与任务号,不含任何旅客信息。
func (s *Service) AssignTask(ctx context.Context, eventID, segmentID, contractor string, actor Actor) (*model.Task, error) {
	var task model.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		ev, err := s.st.GetEventForUpdate(ctx, tx, eventID)
		if err != nil {
			return err
		}
		if ev.Status == model.EventClosed {
			return fmt.Errorf("%w: 事件已闭环", store.ErrPrecondition)
		}
		cur, err := s.st.CurrentClosure(ctx, tx, eventID)
		if err != nil {
			return err
		}
		inClosure := false
		for _, seg := range cur.Segments {
			if seg == segmentID {
				inClosure = true
				break
			}
		}
		if !inClosure {
			return fmt.Errorf("%w: 区段 %s 不在当前封闭边界内", store.ErrPrecondition, segmentID)
		}
		task = model.Task{EventID: eventID, SegmentID: segmentID, Contractor: contractor, AssignedBy: actor.Name}
		if err := s.st.InsertTask(ctx, tx, &task); err != nil {
			return err
		}
		if ev.Status == model.EventOpen {
			ev.Status = model.EventClearing
			if err := s.st.UpdateEvent(ctx, tx, &ev); err != nil {
				return err
			}
		}
		n := model.Notification{EventID: eventID, Channel: "contractor",
			Subject: "removal_task_assigned",
			Payload: map[string]any{"task_id": task.ID, "segment_id": segmentID, "contractor": contractor}}
		if err := s.st.InsertNotification(ctx, tx, &n); err != nil {
			return err
		}
		return s.st.Audit(ctx, tx, "task", task.ID, "task_assigned", actor.Name, actor.source(),
			map[string]any{"event_id": eventID, "segment": segmentID, "contractor": contractor})
	})
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// CompleteTask 承包商回报清除完成;只能操作分派给本承包商的任务。
func (s *Service) CompleteTask(ctx context.Context, taskID string, actor Actor) (*model.Task, error) {
	var t model.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = s.st.GetTaskForUpdate(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if actor.Role == model.RoleContractor && t.Contractor != actor.Name {
			return fmt.Errorf("%w: 任务不属于承包商 %s", store.ErrPrecondition, actor.Name)
		}
		if err := s.st.CompleteTask(ctx, tx, taskID); err != nil {
			return err // 重复完成 → ErrConflict
		}
		if err := s.st.Audit(ctx, tx, "task", t.ID, "task_completed", actor.Name, actor.source(),
			map[string]any{"event_id": t.EventID, "segment": t.SegmentID}); err != nil {
			return err
		}
		t.Status = "done"
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ---- 复查 ----

// StartReview 发起针对当前封闭版本的复查;前置:全部清除任务完成。
func (s *Service) StartReview(ctx context.Context, eventID, note string, actor Actor) (*model.Review, error) {
	var r model.Review
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		ev, err := s.st.GetEventForUpdate(ctx, tx, eventID)
		if err != nil {
			return err
		}
		if ev.Status == model.EventClosed {
			return fmt.Errorf("%w: 事件已闭环", store.ErrPrecondition)
		}
		done, err := s.st.AllTasksDone(ctx, tx, eventID)
		if err != nil {
			return err
		}
		if !done {
			return fmt.Errorf("%w: 尚有未完成的清除任务", store.ErrPrecondition)
		}
		// 封闭集合必须被清除任务全覆盖
		cur, err := s.st.CurrentClosure(ctx, tx, eventID)
		if err != nil {
			return err
		}
		uncovered, err := s.st.UncoveredSegments(ctx, tx, eventID, cur.Segments)
		if err != nil {
			return err
		}
		if len(uncovered) > 0 {
			return fmt.Errorf("%w: 封闭区段 %v 尚未分派清除任务", store.ErrPrecondition, uncovered)
		}
		r = model.Review{EventID: eventID, ClosureVersion: ev.ClosureVersion,
			Status: model.ReviewPending, Note: note, CreatedBy: actor.Name}
		if err := s.st.InsertReview(ctx, tx, &r); err != nil {
			return err
		}
		ev.Status = model.EventReviewing
		if err := s.st.UpdateEvent(ctx, tx, &ev); err != nil {
			return err
		}
		return s.st.Audit(ctx, tx, "review", r.ID, "review_started", actor.Name, actor.source(),
			map[string]any{"event_id": eventID, "closure_version": ev.ClosureVersion})
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CompleteReview 复查落结论。迟到的复查(版本已推进)不能越过当前版本。
func (s *Service) CompleteReview(ctx context.Context, reviewID, outcome, note string, actor Actor) (*model.Review, error) {
	if outcome != model.ReviewPassed && outcome != model.ReviewFailed {
		return nil, fmt.Errorf("%w: outcome 须为 passed|failed", store.ErrPrecondition)
	}
	var r model.Review
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		// 加锁顺序统一为 事件 → 复查,避免与报告扩界路径形成循环等待
		var err error
		r, err = s.st.GetReview(ctx, tx, reviewID)
		if err != nil {
			return err
		}
		ev, err := s.st.GetEventForUpdate(ctx, tx, r.EventID)
		if err != nil {
			return err
		}
		r, err = s.st.GetReviewForUpdate(ctx, tx, reviewID)
		if err != nil {
			return err
		}
		if r.Status != model.ReviewPending {
			return fmt.Errorf("%w: 复查已%s,不能重复落结论", store.ErrConflict, r.Status)
		}
		if r.ClosureVersion != ev.ClosureVersion {
			// 边界已推进,这份复查针对旧版本——迟到回执
			return fmt.Errorf("%w: 复查针对封闭版本 %d,当前已是 %d", store.ErrStale, r.ClosureVersion, ev.ClosureVersion)
		}
		if err := s.st.CompleteReview(ctx, tx, reviewID, outcome, note); err != nil {
			return err
		}
		r.Status = outcome
		switch outcome {
		case model.ReviewPassed:
			ev.Status = model.EventConfirming // 等待双确认恢复运行
		case model.ReviewFailed:
			ev.Status = model.EventClearing // 重新清除
		}
		if err := s.st.UpdateEvent(ctx, tx, &ev); err != nil {
			return err
		}
		return s.st.Audit(ctx, tx, "review", r.ID, "review_completed", actor.Name, actor.source(),
			map[string]any{"event_id": r.EventID, "outcome": outcome, "closure_version": r.ClosureVersion})
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ---- 双确认:缩小封闭 / 恢复运行 ----

type ConfirmInput struct {
	Action         string   `json:"action" binding:"required"` // shrink | reopen
	ClosureVersion int      `json:"closure_version" binding:"required"`
	TargetSegments []string `json:"target_segments"` // shrink 必填
}

type ConfirmResult struct {
	Confirmation model.Confirmation `json:"confirmation"`
	Applied      bool               `json:"applied"` // 两份独立确认是否已集齐并生效
	Closure      *model.Closure     `json:"closure,omitempty"`
	EventStatus  string             `json:"event_status"`
}

// Confirm 接收一份确认。规则:
//   - 必须针对事件当前封闭版本,迟到回执(版本落后)拒绝;
//   - 同一角色对同一封闭版本同一动作只能确认一次(唯一约束兜底);
//   - shrink 的两份确认目标边界必须一致,且不得小于证据种子集合;
//   - 集齐 field_ops + ops_control 两份独立确认后才执行状态迁移。
func (s *Service) Confirm(ctx context.Context, closureID string, in ConfirmInput, actor Actor) (*ConfirmResult, error) {
	if actor.Role != model.RoleFieldOps && actor.Role != model.RoleOpsControl {
		return nil, fmt.Errorf("%w: 仅场务与运行控制可确认", store.ErrPrecondition)
	}
	if in.Action != "shrink" && in.Action != "reopen" {
		return nil, fmt.Errorf("%w: action 须为 shrink|reopen", store.ErrPrecondition)
	}
	var res ConfirmResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		cl, err := s.st.GetClosure(ctx, tx, closureID)
		if err != nil {
			return err
		}
		ev, err := s.st.GetEventForUpdate(ctx, tx, cl.EventID)
		if err != nil {
			return err
		}
		// 迟到回执:版本不匹配直接拒绝
		if cl.Version != ev.ClosureVersion || in.ClosureVersion != ev.ClosureVersion || cl.Status != model.ClosureActive {
			return fmt.Errorf("%w: 封闭版本 %d 已不是当前版本 %d", store.ErrStale, in.ClosureVersion, ev.ClosureVersion)
		}
		if ev.Status == model.EventClosed {
			return fmt.Errorf("%w: 事件已闭环", store.ErrPrecondition)
		}

		target := append([]string(nil), in.TargetSegments...)
		sort.Strings(target)
		if in.Action == "shrink" {
			if len(target) == 0 {
				return fmt.Errorf("%w: shrink 需要 target_segments", store.ErrPrecondition)
			}
			// 新边界必须 ⊆ 当前边界,且 ⊇ 证据种子(不能小于最小风险边界)
			if !closure.IsSubset(target, cl.Segments) || closure.Equal(target, cl.Segments) {
				return fmt.Errorf("%w: 目标边界必须是当前封闭的真子集", store.ErrPrecondition)
			}
			if !closure.IsSubset(ev.SeedSegments, target) {
				return fmt.Errorf("%w: 目标边界不能小于证据指向的最小集合 %v", store.ErrPrecondition, ev.SeedSegments)
			}
		} else { // reopen 前置:清除完成 + 有覆盖当前边界的通过复查
			done, err := s.st.AllTasksDone(ctx, tx, cl.EventID)
			if err != nil {
				return err
			}
			if !done {
				return fmt.Errorf("%w: 尚有未完成的清除任务", store.ErrPrecondition)
			}
			covering, err := s.st.HasCoveringPassedReview(ctx, tx, cl.EventID)
			if err != nil {
				return err
			}
			if !covering {
				return fmt.Errorf("%w: 缺少覆盖当前封闭边界的通过复查", store.ErrPrecondition)
			}
		}

		conf := model.Confirmation{ClosureID: closureID, Action: in.Action, Role: actor.Role,
			Actor: actor.Name, TargetSegments: target}
		if err := s.st.InsertConfirmation(ctx, tx, &conf); err != nil {
			return err // 同角色重复回执 → ErrConflict
		}
		res.Confirmation = conf
		if err := s.st.Audit(ctx, tx, "closure", closureID, "confirm_"+in.Action, actor.Name, actor.source(),
			map[string]any{"role": actor.Role, "version": in.ClosureVersion, "target": target}); err != nil {
			return err
		}

		// 检查是否集齐两份独立确认
		confs, err := s.st.ListConfirmations(ctx, tx, closureID, in.Action)
		if err != nil {
			return err
		}
		roles := map[string]model.Confirmation{}
		for _, c := range confs {
			roles[c.Role] = c
		}
		if len(roles) < 2 {
			res.EventStatus = ev.Status
			return nil // 等待另一角色
		}
		// shrink:两份确认的目标必须一致
		if in.Action == "shrink" && !closure.Equal(roles[model.RoleFieldOps].TargetSegments, roles[model.RoleOpsControl].TargetSegments) {
			return fmt.Errorf("%w: 两份确认的目标边界不一致", store.ErrConflict)
		}

		// 生效
		switch in.Action {
		case "shrink":
			if err := s.st.SupersedeClosure(ctx, tx, cl.ID); err != nil {
				return err
			}
			newCl := model.Closure{EventID: ev.ID, Version: cl.Version + 1, Segments: target,
				Kind: "shrunk", Status: model.ClosureActive, CreatedBy: actor.Name}
			if err := s.st.InsertClosure(ctx, tx, &newCl); err != nil {
				return err
			}
			ev.ClosureVersion = newCl.Version
			if err := s.st.UpdateEvent(ctx, tx, &ev); err != nil {
				return err
			}
			if err := s.notifyClosure(ctx, tx, &ev, &newCl, "partial"); err != nil {
				return err
			}
			if err := s.st.Audit(ctx, tx, "closure", newCl.ID, "closure_shrunk", actor.Name, actor.source(),
				map[string]any{"event_id": ev.ID, "version": newCl.Version, "segments": target}); err != nil {
				return err
			}
			res.Closure = &newCl
		case "reopen":
			if err := s.st.LiftClosure(ctx, tx, cl.ID); err != nil {
				return err
			}
			ev.Status = model.EventClosed
			if err := s.st.UpdateEvent(ctx, tx, &ev); err != nil {
				return err
			}
			n := model.Notification{EventID: ev.ID, Channel: "tower", Subject: "runway_reopened",
				Payload: map[string]any{"event_id": ev.ID, "closure_id": cl.ID,
					"closure_version": cl.Version, "segments": cl.Segments, "availability": "available"}}
			if err := s.st.InsertNotification(ctx, tx, &n); err != nil {
				return err
			}
			n2 := n
			n2.Channel = "flight_dispatch"
			if err := s.st.InsertNotification(ctx, tx, &n2); err != nil {
				return err
			}
			if err := s.st.Audit(ctx, tx, "closure", cl.ID, "closure_lifted", actor.Name, actor.source(),
				map[string]any{"event_id": ev.ID, "version": cl.Version}); err != nil {
				return err
			}
			lifted := cl
			lifted.Status = model.ClosureLifted
			res.Closure = &lifted
		}
		res.Applied = true
		res.EventStatus = ev.Status
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// ---- 查询编排 ----

// EventDetail 是事件全链路视图(仅 ops 角色;contractor/tower 走各自裁剪视图)。
type EventDetail struct {
	Event         model.Event          `json:"event"`
	Reports       []model.Report       `json:"reports"`
	Closures      []model.Closure      `json:"closures"`
	Tasks         []model.Task         `json:"tasks"`
	Reviews       []model.Review       `json:"reviews"`
	Confirmations []model.Confirmation `json:"confirmations"`
	Notifications []model.Notification `json:"notifications"`
}

func (s *Service) GetEventDetail(ctx context.Context, eventID string) (*EventDetail, error) {
	ev, err := s.st.GetEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	d := &EventDetail{Event: ev}
	q := s.st.Pool
	if d.Reports, err = s.st.ListReports(ctx, q, eventID); err != nil {
		return nil, err
	}
	if d.Closures, err = s.st.ListClosures(ctx, q, eventID); err != nil {
		return nil, err
	}
	if d.Tasks, err = s.st.ListTasks(ctx, q, eventID); err != nil {
		return nil, err
	}
	if d.Reviews, err = s.st.ListReviews(ctx, q, eventID); err != nil {
		return nil, err
	}
	if d.Confirmations, err = s.st.ListConfirmationsForEvent(ctx, q, eventID); err != nil {
		return nil, err
	}
	if d.Notifications, err = s.st.ListNotifications(ctx, q, eventID); err != nil {
		return nil, err
	}
	return d, nil
}

// TowerStatus 计算塔台视图:每个区段的最终运行结论,带来源(封闭版本/事件/生效时间)。
func (s *Service) TowerStatus(ctx context.Context) ([]model.SegmentStatus, error) {
	_, segs, err := s.loadGraph(ctx, s.st.Pool)
	if err != nil {
		return nil, err
	}
	active, err := s.st.ActiveClosures(ctx)
	if err != nil {
		return nil, err
	}
	bySegment := map[string]model.Closure{}
	for _, c := range active {
		for _, seg := range c.Segments {
			bySegment[seg] = c
		}
	}
	out := make([]model.SegmentStatus, 0, len(segs))
	for _, sg := range segs {
		st := model.SegmentStatus{SegmentID: sg.ID, Runway: sg.Runway, Available: true}
		if c, closed := bySegment[sg.ID]; closed {
			st.Available = false
			st.ClosureID = c.ID
			st.ClosureVersion = c.Version
			st.EventID = c.EventID
			st.EffectiveAt = c.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, st)
	}
	return out, nil
}

// FlightImpact 是航班延误反查视图:异物→封闭传播→清除→复查→通知送达。
type FlightImpact struct {
	Delays []model.FlightDelay `json:"delays"`
	Events []*EventDetail      `json:"events"`
}

func (s *Service) GetFlightImpact(ctx context.Context, flightNo string, includePassengerInfo bool) (*FlightImpact, error) {
	delays, err := s.st.FlightDelaysByFlight(ctx, flightNo)
	if err != nil {
		return nil, err
	}
	if !includePassengerInfo {
		for i := range delays {
			delays[i].PassengerInfo = ""
		}
	}
	fi := &FlightImpact{Delays: delays}
	seen := map[string]bool{}
	for _, d := range delays {
		if seen[d.EventID] {
			continue
		}
		seen[d.EventID] = true
		detail, err := s.GetEventDetail(ctx, d.EventID)
		if err != nil {
			return nil, err
		}
		fi.Events = append(fi.Events, detail)
	}
	return fi, nil
}

// LinkFlightDelay 把一笔航班延误挂到异物事件上。
func (s *Service) LinkFlightDelay(ctx context.Context, flightNo, eventID, reason, passengerInfo string, actor Actor) (*model.FlightDelay, error) {
	var d model.FlightDelay
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.st.GetEventForUpdate(ctx, tx, eventID); err != nil {
			return err
		}
		d = model.FlightDelay{FlightNo: flightNo, EventID: eventID, Reason: reason, PassengerInfo: passengerInfo}
		if err := s.st.InsertFlightDelay(ctx, tx, &d); err != nil {
			return err
		}
		return s.st.Audit(ctx, tx, "flight_delay", d.ID, "delay_linked", actor.Name, actor.source(),
			map[string]any{"flight_no": flightNo, "event_id": eventID})
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}
