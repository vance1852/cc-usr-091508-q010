// Package closure 根据跑道区段相邻关系计算封闭影响范围。
// 纯函数、确定性、可单测:同样的拓扑与种子永远得到同样的封闭集合。
package closure

import "sort"

// Segment 是跑道区段拓扑节点。
type Segment struct {
	ID   string
	Kind string // "runway" | "crossing"(交叉道口,风险更高)
}

// Graph 是区段邻接图(无向)。
type Graph struct {
	Segments map[string]Segment
	Adj      map[string][]string
}

func NewGraph(segs []Segment, edges [][2]string) *Graph {
	g := &Graph{Segments: map[string]Segment{}, Adj: map[string][]string{}}
	for _, s := range segs {
		g.Segments[s.ID] = s
	}
	for _, e := range edges {
		g.Adj[e[0]] = append(g.Adj[e[0]], e[1])
		g.Adj[e[1]] = append(g.Adj[e[1]], e[0])
	}
	return g
}

// depthByRisk 给出风险等级对应的基础扩散深度。
func depthByRisk(risk string) int {
	switch risk {
	case "high":
		return 2
	case "medium":
		return 1
	default: // low
		return 0
	}
}

// Compute 计算封闭区段集合:
//   - 从种子区段按风险等级深度 BFS 扩散;
//   - 若扩散范围触及交叉道口(crossing),自该道口再向外扩一层——
//     交叉道口是滑行路径汇聚点,异物被航空器携带扩散的风险更高。
//
// 返回排序后的去重区段集合,保证输出确定。
func (g *Graph) Compute(seeds []string, risk string) []string {
	depth := depthByRisk(risk)
	closed := map[string]bool{}
	frontier := append([]string(nil), seeds...)

	expand := func(front []string, d int) {
		for step := 0; step < d && len(front) > 0; step++ {
			var next []string
			for _, id := range front {
				for _, nb := range g.Adj[id] {
					if !closed[nb] {
						closed[nb] = true
						next = append(next, nb)
					}
				}
			}
			front = next
		}
	}

	for _, s := range seeds {
		closed[s] = true
	}
	// 先记录第一层邻居,以便识别"触及道口"需要额外扩散的节点
	expand(frontier, depth)

	// 触及交叉道口:自道口再扩一层
	var crossings []string
	for id := range closed {
		if g.Segments[id].Kind == "crossing" {
			crossings = append(crossings, id)
		}
	}
	for _, c := range crossings {
		for _, nb := range g.Adj[c] {
			if !closed[nb] {
				closed[nb] = true
			}
		}
	}

	out := make([]string, 0, len(closed))
	for id := range closed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// IsSuperset 报告 newSet 是否严格包含 oldSet(风险边界扩大)。
func IsSuperset(oldSet, newSet []string) bool {
	if len(newSet) <= len(oldSet) {
		return false
	}
	m := map[string]bool{}
	for _, s := range newSet {
		m[s] = true
	}
	for _, s := range oldSet {
		if !m[s] {
			return false
		}
	}
	return true
}

// IsSubset 报告 a 是否为 b 的子集(用于校验缩小目标合法)。
func IsSubset(a, b []string) bool {
	m := map[string]bool{}
	for _, s := range b {
		m[s] = true
	}
	for _, s := range a {
		if !m[s] {
			return false
		}
	}
	return true
}

// Equal 判断两个集合是否相同(输入均已排序或无序均可)。
func Equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return IsSubset(a, b)
}
