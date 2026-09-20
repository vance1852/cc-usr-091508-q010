// Package store 封装全部 SQL 访问。所有多步写入由 service 层在事务中编排。
package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fodsys/internal/model"
)

// ErrConflict 表示唯一约束冲突(重复回执、重复分派等)。
var ErrConflict = errors.New("conflict")

// ErrStale 表示回执/请求携带的版本落后于当前版本。
var ErrStale = errors.New("stale version")

// ErrNotFound 表示目标行不存在。
var ErrNotFound = errors.New("not found")

// ErrPrecondition 表示业务前置条件不满足(如还有未完成的清除任务)。
var ErrPrecondition = errors.New("precondition failed")

type Store struct {
	Pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// Querier 抽象 pgx 连接与事务,便于方法同时服务于两者。
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---- 拓扑 ----

func (s *Store) LoadGraph(ctx context.Context, q Querier) ([]model.Segment, [][2]string, error) {
	rows, err := q.Query(ctx, `SELECT id, runway, seq, kind FROM runway_segments ORDER BY runway, seq`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var segs []model.Segment
	for rows.Next() {
		var sg model.Segment
		if err := rows.Scan(&sg.ID, &sg.Runway, &sg.Seq, &sg.Kind); err != nil {
			return nil, nil, err
		}
		segs = append(segs, sg)
	}
	erows, err := q.Query(ctx, `SELECT segment_id, neighbor_id FROM segment_adjacency`)
	if err != nil {
		return nil, nil, err
	}
	defer erows.Close()
	var edges [][2]string
	for erows.Next() {
		var a, b string
		if err := erows.Scan(&a, &b); err != nil {
			return nil, nil, err
		}
		edges = append(edges, [2]string{a, b})
	}
	return segs, edges, erows.Err()
}

// ---- 事件 ----

const eventCols = `id, status, seed_segments, risk_level, coalesce(image_signature,''), coalesce(location_desc,''), closure_version, version, created_at, created_by, updated_at`

func scanEvent(row pgx.Row) (model.Event, error) {
	var e model.Event
	err := row.Scan(&e.ID, &e.Status, &e.SeedSegments, &e.RiskLevel, &e.ImageSignature,
		&e.LocationDesc, &e.ClosureVersion, &e.Version, &e.CreatedAt, &e.CreatedBy, &e.UpdatedAt)
	return e, err
}

// GetEventForUpdate 锁定事件行,串行化同一事件上的并发状态迁移。
func (s *Store) GetEventForUpdate(ctx context.Context, q Querier, id string) (model.Event, error) {
	e, err := scanEvent(q.QueryRow(ctx, `SELECT `+eventCols+` FROM fod_events WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func (s *Store) GetEvent(ctx context.Context, id string) (model.Event, error) {
	e, err := scanEvent(s.Pool.QueryRow(ctx, `SELECT `+eventCols+` FROM fod_events WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func (s *Store) ListEvents(ctx context.Context, includeClosed bool) ([]model.Event, error) {
	sql := `SELECT ` + eventCols + ` FROM fod_events`
	if !includeClosed {
		sql += ` WHERE status != 'closed'`
	}
	sql += ` ORDER BY created_at DESC`
	rows, err := s.Pool.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		if err := rows.Scan(&e.ID, &e.Status, &e.SeedSegments, &e.RiskLevel, &e.ImageSignature,
			&e.LocationDesc, &e.ClosureVersion, &e.Version, &e.CreatedAt, &e.CreatedBy, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OpenEventCandidates 返回未闭环事件的关联候选(供报告并案)。
func (s *Store) OpenEventCandidates(ctx context.Context, q Querier) ([]model.Event, error) {
	rows, err := q.Query(ctx, `SELECT `+eventCols+` FROM fod_events WHERE status != 'closed'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		if err := rows.Scan(&e.ID, &e.Status, &e.SeedSegments, &e.RiskLevel, &e.ImageSignature,
			&e.LocationDesc, &e.ClosureVersion, &e.Version, &e.CreatedAt, &e.CreatedBy, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) InsertEvent(ctx context.Context, q Querier, e *model.Event) error {
	return q.QueryRow(ctx, `INSERT INTO fod_events(status, seed_segments, risk_level, image_signature, location_desc, closure_version, created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at, updated_at`,
		e.Status, e.SeedSegments, e.RiskLevel, nullIfEmpty(e.ImageSignature), nullIfEmpty(e.LocationDesc),
		e.ClosureVersion, e.CreatedBy).Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
}

// UpdateEvent 更新事件状态/种子/封闭版本,并推进行乐观锁计数。
func (s *Store) UpdateEvent(ctx context.Context, q Querier, e *model.Event) error {
	tag, err := q.Exec(ctx, `UPDATE fod_events SET status=$2, seed_segments=$3, closure_version=$4,
		version=version+1, updated_at=now() WHERE id=$1`,
		e.ID, e.Status, e.SeedSegments, e.ClosureVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- 报告 ----

func (s *Store) InsertReport(ctx context.Context, q Querier, r *model.Report) error {
	return q.QueryRow(ctx, `INSERT INTO fod_reports(event_id, segment_id, image_signature, reporter, source, matched_existing)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING id, created_at`,
		r.EventID, r.SegmentID, nullIfEmpty(r.ImageSignature), r.Reporter, r.Source, r.MatchedExisting).
		Scan(&r.ID, &r.CreatedAt)
}

func (s *Store) ListReports(ctx context.Context, q Querier, eventID string) ([]model.Report, error) {
	rows, err := q.Query(ctx, `SELECT id, event_id, segment_id, coalesce(image_signature,''), reporter, source, matched_existing, created_at
		FROM fod_reports WHERE event_id=$1 ORDER BY created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Report
	for rows.Next() {
		var r model.Report
		if err := rows.Scan(&r.ID, &r.EventID, &r.SegmentID, &r.ImageSignature, &r.Reporter, &r.Source, &r.MatchedExisting, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- 封闭 ----

const closureCols = `id, event_id, version, segments, kind, status, created_at, created_by`

func (s *Store) InsertClosure(ctx context.Context, q Querier, c *model.Closure) error {
	return q.QueryRow(ctx, `INSERT INTO closures(event_id, version, segments, kind, status, created_by)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING id, created_at`,
		c.EventID, c.Version, c.Segments, c.Kind, c.Status, c.CreatedBy).Scan(&c.ID, &c.CreatedAt)
}

func (s *Store) SupersedeClosure(ctx context.Context, q Querier, closureID string) error {
	_, err := q.Exec(ctx, `UPDATE closures SET status='superseded' WHERE id=$1 AND status='active'`, closureID)
	return err
}

func (s *Store) LiftClosure(ctx context.Context, q Querier, closureID string) error {
	_, err := q.Exec(ctx, `UPDATE closures SET status='lifted' WHERE id=$1 AND status='active'`, closureID)
	return err
}

func (s *Store) GetClosure(ctx context.Context, q Querier, id string) (model.Closure, error) {
	var c model.Closure
	err := q.QueryRow(ctx, `SELECT `+closureCols+` FROM closures WHERE id=$1`, id).
		Scan(&c.ID, &c.EventID, &c.Version, &c.Segments, &c.Kind, &c.Status, &c.CreatedAt, &c.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// CurrentClosure 返回事件当前生效的封闭(版本最高且 active)。
func (s *Store) CurrentClosure(ctx context.Context, q Querier, eventID string) (model.Closure, error) {
	var c model.Closure
	err := q.QueryRow(ctx, `SELECT `+closureCols+` FROM closures
		WHERE event_id=$1 AND status='active' ORDER BY version DESC LIMIT 1`, eventID).
		Scan(&c.ID, &c.EventID, &c.Version, &c.Segments, &c.Kind, &c.Status, &c.CreatedAt, &c.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (s *Store) ListClosures(ctx context.Context, q Querier, eventID string) ([]model.Closure, error) {
	rows, err := q.Query(ctx, `SELECT `+closureCols+` FROM closures WHERE event_id=$1 ORDER BY version`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Closure
	for rows.Next() {
		var c model.Closure
		if err := rows.Scan(&c.ID, &c.EventID, &c.Version, &c.Segments, &c.Kind, &c.Status, &c.CreatedAt, &c.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ActiveClosures 返回全部生效中的封闭(塔台视图数据源)。
func (s *Store) ActiveClosures(ctx context.Context) ([]model.Closure, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+closureCols+` FROM closures WHERE status='active' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Closure
	for rows.Next() {
		var c model.Closure
		if err := rows.Scan(&c.ID, &c.EventID, &c.Version, &c.Segments, &c.Kind, &c.Status, &c.CreatedAt, &c.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- 复查 ----

func (s *Store) InsertReview(ctx context.Context, q Querier, r *model.Review) error {
	return q.QueryRow(ctx, `INSERT INTO reviews(event_id, closure_version, status, note, created_by)
		VALUES($1,$2,$3,$4,$5) RETURNING id, created_at`,
		r.EventID, r.ClosureVersion, r.Status, nullIfEmpty(r.Note), r.CreatedBy).Scan(&r.ID, &r.CreatedAt)
}

func (s *Store) GetReviewForUpdate(ctx context.Context, q Querier, id string) (model.Review, error) {
	var r model.Review
	err := q.QueryRow(ctx, `SELECT id, event_id, closure_version, status, coalesce(note,''), created_at, created_by, completed_at
		FROM reviews WHERE id=$1 FOR UPDATE`, id).
		Scan(&r.ID, &r.EventID, &r.ClosureVersion, &r.Status, &r.Note, &r.CreatedAt, &r.CreatedBy, &r.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// GetReview 不加锁读取,用于先取 event_id 再按 事件→复查 顺序加锁。
func (s *Store) GetReview(ctx context.Context, q Querier, id string) (model.Review, error) {
	var r model.Review
	err := q.QueryRow(ctx, `SELECT id, event_id, closure_version, status, coalesce(note,''), created_at, created_by, completed_at
		FROM reviews WHERE id=$1`, id).
		Scan(&r.ID, &r.EventID, &r.ClosureVersion, &r.Status, &r.Note, &r.CreatedAt, &r.CreatedBy, &r.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func (s *Store) CompleteReview(ctx context.Context, q Querier, id, status, note string) error {
	tag, err := q.Exec(ctx, `UPDATE reviews SET status=$2, note=coalesce(nullif($3,''), note), completed_at=now()
		WHERE id=$1 AND status='pending'`, id, status, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict // 已完成或已失效的复查不能重复落结论
	}
	return nil
}

// SupersedeOpenReviews 边界扩大时立即使未闭环复查(pending/passed)失效。
func (s *Store) SupersedeOpenReviews(ctx context.Context, q Querier, eventID string) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE reviews SET status='superseded'
		WHERE event_id=$1 AND status IN ('pending','passed')`, eventID)
	return tag.RowsAffected(), err
}

func (s *Store) ListReviews(ctx context.Context, q Querier, eventID string) ([]model.Review, error) {
	rows, err := q.Query(ctx, `SELECT id, event_id, closure_version, status, coalesce(note,''), created_at, created_by, completed_at
		FROM reviews WHERE event_id=$1 ORDER BY created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Review
	for rows.Next() {
		var r model.Review
		if err := rows.Scan(&r.ID, &r.EventID, &r.ClosureVersion, &r.Status, &r.Note, &r.CreatedAt, &r.CreatedBy, &r.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasCoveringPassedReview 判定是否存在一次通过的复查,其复查时边界覆盖当前封闭边界。
// 缩小封闭后,此前针对更大边界通过的复查仍然有效。
func (s *Store) HasCoveringPassedReview(ctx context.Context, q Querier, eventID string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM reviews r
			JOIN closures rc ON rc.event_id = r.event_id AND rc.version = r.closure_version
			JOIN closures cur ON cur.event_id = r.event_id AND cur.status = 'active'
			WHERE r.event_id = $1 AND r.status = 'passed'
			  AND rc.segments @> cur.segments
		)`, eventID).Scan(&ok)
	return ok, err
}

// ---- 确认 ----

// InsertConfirmation 写入一份确认;唯一约束拒绝同角色重复回执。
func (s *Store) InsertConfirmation(ctx context.Context, q Querier, c *model.Confirmation) error {
	err := q.QueryRow(ctx, `INSERT INTO closure_confirmations(closure_id, action, role, actor, target_segments)
		VALUES($1,$2,$3,$4,$5) RETURNING id, created_at`,
		c.ClosureID, c.Action, c.Role, c.Actor, nullIfEmptySlice(c.TargetSegments)).Scan(&c.ID, &c.CreatedAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) ListConfirmations(ctx context.Context, q Querier, closureID, action string) ([]model.Confirmation, error) {
	rows, err := q.Query(ctx, `SELECT id, closure_id, action, role, actor, coalesce(target_segments,'{}'), created_at
		FROM closure_confirmations WHERE closure_id=$1 AND action=$2 ORDER BY created_at`, closureID, action)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Confirmation
	for rows.Next() {
		var c model.Confirmation
		if err := rows.Scan(&c.ID, &c.ClosureID, &c.Action, &c.Role, &c.Actor, &c.TargetSegments, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ListConfirmationsForEvent(ctx context.Context, q Querier, eventID string) ([]model.Confirmation, error) {
	rows, err := q.Query(ctx, `SELECT c.id, c.closure_id, c.action, c.role, c.actor, coalesce(c.target_segments,'{}'), c.created_at
		FROM closure_confirmations c JOIN closures cl ON cl.id = c.closure_id
		WHERE cl.event_id=$1 ORDER BY c.created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Confirmation
	for rows.Next() {
		var c model.Confirmation
		if err := rows.Scan(&c.ID, &c.ClosureID, &c.Action, &c.Role, &c.Actor, &c.TargetSegments, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- 清除任务 ----

func (s *Store) InsertTask(ctx context.Context, q Querier, t *model.Task) error {
	err := q.QueryRow(ctx, `INSERT INTO removal_tasks(event_id, segment_id, contractor, assigned_by)
		VALUES($1,$2,$3,$4) RETURNING id, assigned_at`,
		t.EventID, t.SegmentID, t.Contractor, t.AssignedBy).Scan(&t.ID, &t.AssignedAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) GetTaskForUpdate(ctx context.Context, q Querier, id string) (model.Task, error) {
	var t model.Task
	err := q.QueryRow(ctx, `SELECT id, event_id, segment_id, contractor, status, assigned_at, assigned_by, completed_at
		FROM removal_tasks WHERE id=$1 FOR UPDATE`, id).
		Scan(&t.ID, &t.EventID, &t.SegmentID, &t.Contractor, &t.Status, &t.AssignedAt, &t.AssignedBy, &t.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (s *Store) CompleteTask(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `UPDATE removal_tasks SET status='done', completed_at=now()
		WHERE id=$1 AND status='assigned'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict // 重复完成回执
	}
	return nil
}

func (s *Store) ListTasks(ctx context.Context, q Querier, eventID string) ([]model.Task, error) {
	rows, err := q.Query(ctx, `SELECT id, event_id, segment_id, contractor, status, assigned_at, assigned_by, completed_at
		FROM removal_tasks WHERE event_id=$1 ORDER BY assigned_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Task
	for rows.Next() {
		var t model.Task
		if err := rows.Scan(&t.ID, &t.EventID, &t.SegmentID, &t.Contractor, &t.Status, &t.AssignedAt, &t.AssignedBy, &t.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ListTasksByContractor(ctx context.Context, contractor string) ([]model.Task, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, event_id, segment_id, contractor, status, assigned_at, assigned_by, completed_at
		FROM removal_tasks WHERE contractor=$1 ORDER BY assigned_at DESC`, contractor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Task
	for rows.Next() {
		var t model.Task
		if err := rows.Scan(&t.ID, &t.EventID, &t.SegmentID, &t.Contractor, &t.Status, &t.AssignedAt, &t.AssignedBy, &t.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AllTasksDone 判定事件是否没有未完成清除任务。
func (s *Store) AllTasksDone(ctx context.Context, q Querier, eventID string) (bool, error) {
	var pending int
	err := q.QueryRow(ctx, `SELECT count(*) FROM removal_tasks WHERE event_id=$1 AND status='assigned'`, eventID).Scan(&pending)
	return pending == 0, err
}

// UncoveredSegments 返回封闭集合中尚无清除任务的区段。
func (s *Store) UncoveredSegments(ctx context.Context, q Querier, eventID string, segments []string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT seg FROM unnest($2::text[]) AS seg
		WHERE NOT EXISTS (SELECT 1 FROM removal_tasks t WHERE t.event_id=$1 AND t.segment_id=seg)`, eventID, segments)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- 通知 ----

func (s *Store) InsertNotification(ctx context.Context, q Querier, n *model.Notification) error {
	payload, err := json.Marshal(n.Payload)
	if err != nil {
		return err
	}
	return q.QueryRow(ctx, `INSERT INTO notifications(event_id, channel, subject, payload)
		VALUES($1,$2,$3,$4) RETURNING id, created_at`,
		n.EventID, n.Channel, n.Subject, payload).Scan(&n.ID, &n.CreatedAt)
}

// PendingNotifications 供派发 worker 轮询;worker 无内存队列,重启后自然续发。
func (s *Store) PendingNotifications(ctx context.Context, limit int) ([]model.Notification, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, event_id, channel, subject, payload, status, attempts, created_at, sent_at, delivered_at, coalesce(delivered_by,'')
		FROM notifications WHERE status='pending' ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNotifications(rows)
}

func (s *Store) MarkNotificationSent(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE notifications SET status='sent', sent_at=now(), attempts=attempts+1
		WHERE id=$1 AND status='pending'`, id)
	return err
}

func (s *Store) AckNotification(ctx context.Context, id, actor string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE notifications SET status='delivered', delivered_at=now(), delivered_by=$2
		WHERE id=$1 AND status='sent'`, id, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

func (s *Store) ListNotifications(ctx context.Context, q Querier, eventID string) ([]model.Notification, error) {
	rows, err := q.Query(ctx, `SELECT id, event_id, channel, subject, payload, status, attempts, created_at, sent_at, delivered_at, coalesce(delivered_by,'')
		FROM notifications WHERE event_id=$1 ORDER BY created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNotifications(rows)
}

func scanNotifications(rows pgx.Rows) ([]model.Notification, error) {
	var out []model.Notification
	for rows.Next() {
		var n model.Notification
		var payload []byte
		err := rows.Scan(&n.ID, &n.EventID, &n.Channel, &n.Subject, &payload, &n.Status, &n.Attempts,
			&n.CreatedAt, &n.SentAt, &n.DeliveredAt, &n.DeliveredBy)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &n.Payload); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---- 航班延误 ----

func (s *Store) InsertFlightDelay(ctx context.Context, q Querier, d *model.FlightDelay) error {
	return q.QueryRow(ctx, `INSERT INTO flight_delays(flight_no, event_id, reason, passenger_info)
		VALUES($1,$2,$3,$4) RETURNING id, created_at`,
		d.FlightNo, d.EventID, d.Reason, nullIfEmpty(d.PassengerInfo)).Scan(&d.ID, &d.CreatedAt)
}

func (s *Store) FlightDelaysByFlight(ctx context.Context, flightNo string) ([]model.FlightDelay, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, flight_no, event_id, reason, coalesce(passenger_info,''), created_at
		FROM flight_delays WHERE flight_no=$1 ORDER BY created_at`, flightNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FlightDelay
	for rows.Next() {
		var d model.FlightDelay
		if err := rows.Scan(&d.ID, &d.FlightNo, &d.EventID, &d.Reason, &d.PassengerInfo, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- 审计 ----

func (s *Store) Audit(ctx context.Context, q Querier, entity, entityID, action, actor, source string, detail any) error {
	var payload []byte
	var err error
	if detail != nil {
		payload, err = json.Marshal(detail)
		if err != nil {
			return err
		}
	}
	_, err = q.Exec(ctx, `INSERT INTO audit_log(entity, entity_id, action, actor, source, detail)
		VALUES($1,$2,$3,$4,$5,$6)`, entity, entityID, action, actor, source, payload)
	return err
}

func (s *Store) ListAudit(ctx context.Context, entity, entityID string) ([]model.AuditEntry, error) {
	sql := `SELECT id, entity, entity_id, action, actor, source, detail, created_at FROM audit_log`
	args := []any{}
	if entity != "" {
		sql += ` WHERE entity=$1 AND entity_id=$2`
		args = append(args, entity, entityID)
	}
	sql += ` ORDER BY id`
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditEntry
	for rows.Next() {
		var a model.AuditEntry
		var detail []byte
		if err := rows.Scan(&a.ID, &a.Entity, &a.EntityID, &a.Action, &a.Actor, &a.Source, &detail, &a.CreatedAt); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &a.Detail)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfEmptySlice(s []string) any {
	if len(s) == 0 {
		return nil
	}
	return s
}
