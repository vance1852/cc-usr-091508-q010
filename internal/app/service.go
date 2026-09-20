package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hashicorp/go-uuid"
	"github.com/lib/pq"
)

type Service struct {
	db *sql.DB
}

func New(db *sql.DB) *Service { return &Service{db: db} }

func newID(prefix string) string {
	u, _ := uuid.GenerateUUID()
	return prefix + "-" + u[:8] + u[9:13]
}

// pqArray 让 []string 以 TEXT[] 形式被 database/sql 扫描。
func pqArray(p *[]string) *pq.StringArray {
	return (*pq.StringArray)(p)
}

// ---------- 通用辅助 ----------

func (s *Service) audit(tx *sql.Tx, entityType, entityID, action string, version interface{}, source, actor string, detail interface{}) {
	b, _ := json.Marshal(detail)
	var v sql.NullInt64
	switch x := version.(type) {
	case int:
		v = sql.NullInt64{Int64: int64(x), Valid: true}
	case int64:
		v = sql.NullInt64{Int64: x, Valid: true}
	}
	_, _ = tx.Exec(`INSERT INTO audit_log(entity_type,entity_id,action,version,detail,source,actor)
	 VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7)`, entityType, entityID, action, v, string(b), source, actor)
}

// loadAdjacency 加载机场内区段相邻边（无向图，表里存双向行）。
func (s *Service) loadAdjacency(tx *sql.Tx, airportID string) (map[string]map[string]string, error) {
	rows, err := tx.Query(`
	 SELECT a.segment_id, a.adjacent_segment, a.kind
	   FROM segment_adjacency a JOIN runway_segments rs ON rs.id = a.segment_id
	  WHERE rs.airport_id = $1`, airportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	g := map[string]map[string]string{}
	for rows.Next() {
		var from, to, kind string
		if err := rows.Scan(&from, &to, &kind); err != nil {
			return nil, err
		}
		if g[from] == nil {
			g[from] = map[string]string{}
		}
		g[from][to] = kind
	}
	return g, rows.Err()
}

// bfsClosure 以 start 为中心按相邻关系传播 radius 跳，返回到达区段及传播边依据。
// 交叉道口 (INTERSECTION) 与同跑道相邻 (NEXT_ON_RUNWAY) 同等可通行——FOD 风险沿物理连通传播。
func bfsClosure(g map[string]map[string]string, start string, radius int) (map[string]int, []PropEdge) {
	dist := map[string]int{start: 0}
	edges := []PropEdge{}
	if radius <= 0 {
		return dist, edges
	}
	frontier := []string{start}
	for hops := 1; hops <= radius; hops++ {
		var next []string
		for _, cur := range frontier {
			for nb, kind := range g[cur] {
				if _, seen := dist[nb]; seen {
					continue
				}
				dist[nb] = hops
				edges = append(edges, PropEdge{FromSegment: cur, ToSegment: nb, EdgeKind: kind, Hops: hops})
				next = append(next, nb)
			}
		}
		if len(next) == 0 {
			break
		}
		frontier = next
	}
	return dist, edges
}

// closureSet 支持多起点（SPANS_SEGMENTS/交叉道口两端同时作为起点）。
// 按入参顺序处理起点并以 to_segment 去重：交叉道口参照点的首次到达边
// （如 S3->T2 INTERSECTION）得以保留。
func closureSet(g map[string]map[string]string, starts []string, radius int) ([]string, []PropEdge) {
	all := map[string]int{}
	var edges []PropEdge
	edgeSeen := map[string]bool{}
	for _, st := range starts {
		d, e := bfsClosure(g, st, radius)
		for seg, hops := range d {
			if cur, ok := all[seg]; !ok || hops < cur {
				all[seg] = hops
			}
		}
		for _, pe := range e {
			if !edgeSeen[pe.ToSegment] {
				edgeSeen[pe.ToSegment] = true
				edges = append(edges, pe)
			}
		}
	}
	out := make([]string, 0, len(all))
	for seg := range all {
		out = append(out, seg)
	}
	sort.Strings(out)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Hops != edges[j].Hops {
			return edges[i].Hops < edges[j].Hops
		}
		return edges[i].ToSegment < edges[j].ToSegment
	})
	return out, edges
}

// versionRow 是事件当前版本的完整快照。
type versionRow struct {
	ID             int64
	EventID        string
	Version        int
	ChangeKind     string
	SegmentID      string
	RefSegmentID   sql.NullString
	LocationQual   string
	RiskRadius     int
	ClosedSegments []string
	Decision       string
	State          string
}

func (s *Service) currentVersion(tx *sql.Tx, eventID string, forUpdate bool) (versionRow, error) {
	q := `SELECT id,event_id,version,change_kind,segment_id,ref_segment_id,location_qual,
	                risk_radius_seg,closed_segments,decision,decision_state
	         FROM event_versions WHERE event_id=$1 ORDER BY version DESC LIMIT 1`
	if forUpdate {
		// ORDER BY ... LIMIT 1 + FOR UPDATE：锁住最新版本行；事件行也由调用方 FOR UPDATE 锁住。
		q = `SELECT id,event_id,version,change_kind,segment_id,ref_segment_id,location_qual,
	                risk_radius_seg,closed_segments,decision,decision_state
	         FROM event_versions WHERE event_id=$1 ORDER BY version DESC LIMIT 1 FOR UPDATE`
	}
	var v versionRow
	err := tx.QueryRow(q, eventID).Scan(&v.ID, &v.EventID, &v.Version, &v.ChangeKind, &v.SegmentID,
		&v.RefSegmentID, &v.LocationQual, &v.RiskRadius, pqArray(&v.ClosedSegments), &v.Decision, &v.State)
	if err != nil {
		return v, err
	}
	return v, nil
}

// latestFinalVersion 返回当前对外生效的版本（跳过 PENDING 的缩小/恢复提案）。
func (s *Service) latestFinalVersion(tx *sql.Tx, eventID string) (versionRow, error) {
	q := `SELECT id,event_id,version,change_kind,segment_id,ref_segment_id,location_qual,
	                risk_radius_seg,closed_segments,decision,decision_state
	         FROM event_versions WHERE event_id=$1 AND decision_state='FINAL'
	     ORDER BY version DESC LIMIT 1`
	var v versionRow
	err := tx.QueryRow(q, eventID).Scan(&v.ID, &v.EventID, &v.Version, &v.ChangeKind, &v.SegmentID,
		&v.RefSegmentID, &v.LocationQual, &v.RiskRadius, pqArray(&v.ClosedSegments), &v.Decision, &v.State)
	return v, err
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func diffSet(a, b []string) (onlyA, onlyB []string) {
	inB := map[string]bool{}
	for _, x := range b {
		inB[x] = true
	}
	inA := map[string]bool{}
	for _, x := range a {
		inA[x] = true
		if !inB[x] {
			onlyA = append(onlyA, x)
		}
	}
	for _, x := range b {
		if !inA[x] {
			onlyB = append(onlyB, x)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return
}

func errf(code, msg string, args ...interface{}) *DomainError {
	return &DomainError{Code: code, Message: fmt.Sprintf(msg, args...)}
}

type DomainError struct {
	Code    string
	Message string
}

func (e *DomainError) Error() string { return e.Code + ": " + e.Message }
