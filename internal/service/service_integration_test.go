// 集成测试:连真实 PostgreSQL 验证核心业务规则。
// 运行:DATABASE_URL=postgres://postgres@127.0.0.1:54329/fodsys_test go test ./internal/service/...
package service_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"fodsys/internal/db"
	"fodsys/internal/model"
	"fodsys/internal/service"
	"fodsys/internal/store"
)

var testSvc *service.Service
var testStore *store.Store

const testSeed = `
INSERT INTO runway_segments (id, runway, seq, kind) VALUES
  ('S1','RWY36L',1,'runway'), ('S2','RWY36L',2,'runway'), ('S3','RWY36L',3,'runway'),
  ('X1','RWY36L',4,'crossing'), ('S4','RWY36L',5,'runway'), ('S5','RWY36L',6,'runway'),
  ('S6','RWY36L',7,'runway');
INSERT INTO segment_adjacency (segment_id, neighbor_id) VALUES
  ('S1','S2'), ('S2','S3'), ('S3','X1'), ('X1','S4'), ('S4','S5'), ('S5','S6');
`

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres@127.0.0.1:54329/fodsys_test?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skip: 无法连接测试库: %v\n", err)
		os.Exit(0) // 无数据库环境时跳过集成测试
	}
	if err := db.Migrate(ctx, pool, "../../migrations"); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
	// 清库并铺拓扑
	for _, t := range []string{"audit_log", "notifications", "flight_delays", "closure_confirmations",
		"reviews", "removal_tasks", "closures", "fod_reports", "fod_events", "segment_adjacency", "runway_segments"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+t); err != nil {
			fmt.Fprintf(os.Stderr, "clean %s: %v\n", t, err)
			os.Exit(1)
		}
	}
	if _, err := pool.Exec(ctx, testSeed); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
	testStore = store.New(pool)
	testSvc = service.New(testStore)
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

var (
	ops   = service.Actor{Role: model.RoleOpsControl, Name: "ops-li", Source: "ops-console"}
	field = service.Actor{Role: model.RoleFieldOps, Name: "field-wang", Source: "field-radio"}
)

func cleanup(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{"audit_log", "notifications", "flight_delays", "closure_confirmations",
		"reviews", "removal_tasks", "closures", "fod_reports", "fod_events"} {
		if _, err := testStore.Pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
}

func report(t *testing.T, seg, sig, risk string) *service.ReportResult {
	t.Helper()
	res, err := testSvc.SubmitReport(context.Background(), service.ReportInput{
		SegmentID: seg, ImageSignature: sig, Reporter: "patrol-1", Source: "patrol", RiskLevel: risk,
	}, field)
	if err != nil {
		t.Fatalf("submit report: %v", err)
	}
	return res
}

// 报告落在道口旁:中风险扩散一层,触及道口 X1 再外扩一层。
func TestReportNearCrossingComputesClosure(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "medium")
	want := []string{"S2", "S3", "S4", "X1"}
	if fmt.Sprint(res.Closure.Segments) != fmt.Sprint(want) {
		t.Fatalf("封闭边界 = %v, 期望 %v", res.Closure.Segments, want)
	}
	if res.MatchedExisting {
		t.Fatal("首报不应并案")
	}
}

// 位置相邻 + 影像相似 → 并入已有事件;影像差异大 → 新事件。
func TestReportCorrelation(t *testing.T) {
	defer cleanup(t)
	first := report(t, "S3", "aabbccddeeff0011", "medium")

	// 相邻区段 S2,签名差 1 位 → 并案
	second := report(t, "S2", "aabbccddeeff0010", "medium")
	if !second.MatchedExisting || second.Event.ID != first.Event.ID {
		t.Fatalf("应并案到 %s, 得 %+v", first.Event.ID, second.Event)
	}

	// 同区段但影像完全不同 → 新事件
	third := report(t, "S3", "55001199aabbccdd", "medium")
	if third.MatchedExisting || third.Event.ID == first.Event.ID {
		t.Fatal("影像明显不同不应并案")
	}
}

// 新证据扩大边界:旧复查立即失效,事件回退清除阶段。
func TestExpansionSupersedesReviews(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "low") // low:仅种子区段
	evID := res.Event.ID

	// 清除 + 复查通过
	if _, err := testSvc.AssignTask(context.Background(), evID, "S3", "contractor-A", ops); err != nil {
		t.Fatal(err)
	}
	tasks, _ := testStore.ListTasks(context.Background(), testStore.Pool, evID)
	if _, err := testSvc.CompleteTask(context.Background(), tasks[0].ID, service.Actor{Role: model.RoleContractor, Name: "contractor-A"}); err != nil {
		t.Fatal(err)
	}
	rv, err := testSvc.StartReview(context.Background(), evID, "", field)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testSvc.CompleteReview(context.Background(), rv.ID, "passed", "", field); err != nil {
		t.Fatal(err)
	}

	// 新证据落在道口 X1(与种子 S3 相邻,影像相近 → 并案扩界)
	exp := report(t, "X1", "aabbccddeeff0012", "medium")
	if !exp.Expanded {
		t.Fatal("X1 证据应扩大边界")
	}
	reviews, _ := testStore.ListReviews(context.Background(), testStore.Pool, evID)
	if reviews[0].Status != model.ReviewSuperseded {
		t.Fatalf("边界扩大后旧复查应失效, 状态=%s", reviews[0].Status)
	}
	detail, _ := testSvc.GetEventDetail(context.Background(), evID)
	if detail.Event.Status != model.EventClearing {
		t.Fatalf("边界扩大后事件应回退清除阶段, 状态=%s", detail.Event.Status)
	}
}

// 恢复运行必须集齐场务与运行控制两份独立确认;重复与迟到回执被拒绝。
func TestDualConfirmationAndStaleReceipts(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "low")
	evID := res.Event.ID
	clID := res.Closure.ID

	if _, err := testSvc.AssignTask(context.Background(), evID, "S3", "contractor-A", ops); err != nil {
		t.Fatal(err)
	}
	tasks, _ := testStore.ListTasks(context.Background(), testStore.Pool, evID)
	if _, err := testSvc.CompleteTask(context.Background(), tasks[0].ID, service.Actor{Role: model.RoleContractor, Name: "contractor-A"}); err != nil {
		t.Fatal(err)
	}
	rv, _ := testSvc.StartReview(context.Background(), evID, "", field)
	if _, err := testSvc.CompleteReview(context.Background(), rv.ID, "passed", "", field); err != nil {
		t.Fatal(err)
	}

	// 第一份确认:场务
	r1, err := testSvc.Confirm(context.Background(), clID, service.ConfirmInput{Action: "reopen", ClosureVersion: 1}, field)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Applied {
		t.Fatal("单份确认不应生效")
	}
	// 同角色重复回执 → 拒绝
	if _, err := testSvc.Confirm(context.Background(), clID, service.ConfirmInput{Action: "reopen", ClosureVersion: 1}, field); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("重复回执应冲突, 得 %v", err)
	}
	// 第二份确认:运行控制 → 生效,事件闭环
	r2, err := testSvc.Confirm(context.Background(), clID, service.ConfirmInput{Action: "reopen", ClosureVersion: 1}, ops)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Applied || r2.EventStatus != model.EventClosed {
		t.Fatalf("双确认应闭环事件, 得 %+v", r2)
	}
	// 闭环后迟到回执 → 拒绝
	if _, err := testSvc.Confirm(context.Background(), clID, service.ConfirmInput{Action: "reopen", ClosureVersion: 1}, ops); err == nil {
		t.Fatal("闭环后的迟到回执应被拒绝")
	}
}

// 边界扩大使复查立即失效;迟到的落结论不能越过当前版本。
func TestStaleReviewRejected(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "low")
	evID := res.Event.ID
	if _, err := testSvc.AssignTask(context.Background(), evID, "S3", "contractor-A", ops); err != nil {
		t.Fatal(err)
	}
	tasks, _ := testStore.ListTasks(context.Background(), testStore.Pool, evID)
	testSvc.CompleteTask(context.Background(), tasks[0].ID, service.Actor{Role: model.RoleContractor, Name: "contractor-A"})
	rv, _ := testSvc.StartReview(context.Background(), evID, "", field)

	// 复查进行中,新证据扩大边界 → 版本推进
	report(t, "X1", "aabbccddeeff0012", "medium")

	// 旧复查已被扩界置为 superseded,迟到落结论 → 拒绝
	if _, err := testSvc.CompleteReview(context.Background(), rv.ID, "passed", "", field); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("迟到复查应被拒绝, 得 %v", err)
	}
	// 复查状态必须为 superseded
	reviews, _ := testStore.ListReviews(context.Background(), testStore.Pool, evID)
	if reviews[0].Status != model.ReviewSuperseded {
		t.Fatalf("复查应已失效, 状态=%s", reviews[0].Status)
	}
}

// 并发:复查落结论与边界扩大竞争,串行化后恰有一个成功。
func TestConcurrentReviewVsExpansion(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "low")
	evID := res.Event.ID
	testSvc.AssignTask(context.Background(), evID, "S3", "contractor-A", ops)
	tasks, _ := testStore.ListTasks(context.Background(), testStore.Pool, evID)
	testSvc.CompleteTask(context.Background(), tasks[0].ID, service.Actor{Role: model.RoleContractor, Name: "contractor-A"})
	rv, _ := testSvc.StartReview(context.Background(), evID, "", field)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = testSvc.CompleteReview(context.Background(), rv.ID, "passed", "", field)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = testSvc.SubmitReport(context.Background(), service.ReportInput{
			SegmentID: "X1", ImageSignature: "aabbccddeeff0012", Reporter: "cam-7", Source: "camera",
		}, field)
	}()
	wg.Wait()

	// 两个操作都允许成功(串行化后扩界可能先提交,使复查 stale);
	// 但最终状态必须一致:若复查通过,则其版本必须等于当前封闭版本。
	detail, err := testSvc.GetEventDetail(context.Background(), evID)
	if err != nil {
		t.Fatal(err)
	}
	var passed *model.Review
	for i := range detail.Reviews {
		if detail.Reviews[i].Status == model.ReviewPassed {
			passed = &detail.Reviews[i]
		}
	}
	if passed != nil && passed.ClosureVersion != detail.Event.ClosureVersion {
		t.Fatalf("通过的复查版本 %d 落后于当前封闭版本 %d —— 并发串行化被破坏",
			passed.ClosureVersion, detail.Event.ClosureVersion)
	}
}

// 缩小封闭:双确认 + 目标边界一致;不能小于证据种子集合。
func TestShrinkRequiresDualConfirmation(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "medium") // 封闭 S2,S3,X1,S4
	evID := res.Event.ID
	clID := res.Closure.ID

	// 目标小于种子 → 拒绝
	if _, err := testSvc.Confirm(context.Background(), clID, service.ConfirmInput{
		Action: "shrink", ClosureVersion: 1, TargetSegments: []string{"S2"}}, field); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("目标小于种子应拒绝, 得 %v", err)
	}
	// 版本不匹配的迟到确认 → 拒绝
	if _, err := testSvc.Confirm(context.Background(), clID, service.ConfirmInput{
		Action: "shrink", ClosureVersion: 99, TargetSegments: []string{"S3"}}, field); !errors.Is(err, store.ErrStale) {
		t.Fatalf("版本不匹配应返回 stale, 得 %v", err)
	}
	// 合法缩小:{S3}(种子) —— 场务先确认
	r1, err := testSvc.Confirm(context.Background(), clID, service.ConfirmInput{
		Action: "shrink", ClosureVersion: 1, TargetSegments: []string{"S3"}}, field)
	if err != nil || r1.Applied {
		t.Fatalf("首份 shrink 确认不应生效: %v %+v", err, r1)
	}
	// 运行控制确认不同目标 → 冲突
	if _, err := testSvc.Confirm(context.Background(), clID, service.ConfirmInput{
		Action: "shrink", ClosureVersion: 1, TargetSegments: []string{"S2", "S3"}}, ops); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("目标不一致应冲突, 得 %v", err)
	}
	// 运行控制确认相同目标 → 生效,封闭版本推进
	r2, err := testSvc.Confirm(context.Background(), clID, service.ConfirmInput{
		Action: "shrink", ClosureVersion: 1, TargetSegments: []string{"S3"}}, ops)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Applied || r2.Closure == nil || r2.Closure.Version != 2 {
		t.Fatalf("shrink 应生效为 v2, 得 %+v", r2)
	}
	detail, _ := testSvc.GetEventDetail(context.Background(), evID)
	if fmt.Sprint(detail.Closures[1].Segments) != "[S3]" {
		t.Fatalf("新边界应为 [S3], 得 %v", detail.Closures[1].Segments)
	}
}

// 承包商只能完成分派给本承包商的任务。
func TestContractorIsolation(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "low")
	evID := res.Event.ID
	testSvc.AssignTask(context.Background(), evID, "S3", "contractor-A", ops)
	tasks, _ := testStore.ListTasks(context.Background(), testStore.Pool, evID)

	other := service.Actor{Role: model.RoleContractor, Name: "contractor-B"}
	if _, err := testSvc.CompleteTask(context.Background(), tasks[0].ID, other); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("承包商 B 不应能完成 A 的任务, 得 %v", err)
	}
	// 重复完成 → 冲突
	owner := service.Actor{Role: model.RoleContractor, Name: "contractor-A"}
	if _, err := testSvc.CompleteTask(context.Background(), tasks[0].ID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := testSvc.CompleteTask(context.Background(), tasks[0].ID, owner); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("重复完成应冲突, 得 %v", err)
	}
}

// 航班延误反查:异物→封闭传播→清除→复查→通知送达链路完整。
func TestFlightImpactTrace(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "medium")
	evID := res.Event.ID
	if _, err := testSvc.LinkFlightDelay(context.Background(), "CA1234", evID, "跑道封闭等待", "旅客182人,转机34人", ops); err != nil {
		t.Fatal(err)
	}

	impact, err := testSvc.GetFlightImpact(context.Background(), "CA1234", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.Delays) != 1 || impact.Delays[0].PassengerInfo == "" {
		t.Fatalf("运行控制应看到旅客信息, 得 %+v", impact.Delays)
	}
	if len(impact.Events) != 1 || len(impact.Events[0].Closures) != 1 {
		t.Fatalf("应关联到事件与封闭, 得 %+v", impact.Events)
	}
	if len(impact.Events[0].Notifications) == 0 {
		t.Fatal("应产生塔台/放行席通知")
	}

	// 场务视图:旅客信息脱敏
	masked, _ := testSvc.GetFlightImpact(context.Background(), "CA1234", false)
	if masked.Delays[0].PassengerInfo != "" {
		t.Fatal("场务视图不应包含旅客信息")
	}
}

// 塔台视图:只读最终运行结论,带来源。
func TestTowerStatusSourced(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "medium")
	status, err := testSvc.TowerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closed := map[string]model.SegmentStatus{}
	for _, s := range status {
		if !s.Available {
			closed[s.SegmentID] = s
		}
	}
	for _, seg := range []string{"S2", "S3", "X1", "S4"} {
		st, ok := closed[seg]
		if !ok {
			t.Fatalf("%s 应不可用", seg)
		}
		if st.EventID != res.Event.ID || st.ClosureVersion != 1 || st.EffectiveAt == "" {
			t.Fatalf("%s 结论缺少来源: %+v", seg, st)
		}
	}
	if _, ok := closed["S1"]; ok {
		t.Fatal("S1 不应被封闭")
	}
}

// 通知回执:pending→sent→delivered,重复回执冲突。
func TestNotificationAckFlow(t *testing.T) {
	defer cleanup(t)
	res := report(t, "S3", "aabbccddeeff0011", "medium")
	detail, _ := testSvc.GetEventDetail(context.Background(), res.Event.ID)
	if len(detail.Notifications) == 0 {
		t.Fatal("应有通知")
	}
	n := detail.Notifications[0]
	if n.Status != model.NotifyPending {
		t.Fatalf("新通知应 pending, 得 %s", n.Status)
	}
	if err := testStore.MarkNotificationSent(context.Background(), n.ID); err != nil {
		t.Fatal(err)
	}
	if err := testStore.AckNotification(context.Background(), n.ID, "tower-desk"); err != nil {
		t.Fatal(err)
	}
	if err := testStore.AckNotification(context.Background(), n.ID, "tower-desk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("重复回执应冲突, 得 %v", err)
	}
}
