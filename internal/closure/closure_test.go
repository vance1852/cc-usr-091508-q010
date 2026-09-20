package closure

import (
	"reflect"
	"testing"
)

// 测试拓扑:S1-S2-S3-X1-S4-S5-S6,X1 为交叉道口。
func testGraph() *Graph {
	segs := []Segment{
		{ID: "S1"}, {ID: "S2"}, {ID: "S3"}, {ID: "X1", Kind: "crossing"},
		{ID: "S4"}, {ID: "S5"}, {ID: "S6"},
	}
	edges := [][2]string{
		{"S1", "S2"}, {"S2", "S3"}, {"S3", "X1"}, {"X1", "S4"}, {"S4", "S5"}, {"S5", "S6"},
	}
	return NewGraph(segs, edges)
}

func TestComputeLowRiskOnlySeed(t *testing.T) {
	g := testGraph()
	got := g.Compute([]string{"S3"}, "low")
	// 低风险不扩散,但种子 S3 邻接道口?不——S3 本身不是道口,不触发额外扩散
	want := []string{"S3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestComputeMediumRiskSpreadsOneHop(t *testing.T) {
	g := testGraph()
	got := g.Compute([]string{"S2"}, "medium")
	want := []string{"S1", "S2", "S3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestComputeCrossingTriggersExtraHop(t *testing.T) {
	g := testGraph()
	// S3 中风险扩散到 X1(道口),道口再外扩一层到 S4;输出按字典序排序
	got := g.Compute([]string{"S3"}, "medium")
	want := []string{"S2", "S3", "S4", "X1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestComputeHighRiskTwoHops(t *testing.T) {
	g := testGraph()
	got := g.Compute([]string{"S2"}, "high")
	// S2 -> S1,S3 -> X1(再触发道口外扩 S4);输出按字典序排序
	want := []string{"S1", "S2", "S3", "S4", "X1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestComputeDeterministicAndSorted(t *testing.T) {
	g := testGraph()
	a := g.Compute([]string{"S5", "S3"}, "high")
	b := g.Compute([]string{"S3", "S5"}, "high")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("非确定性输出: %v vs %v", a, b)
	}
}

func TestIsSuperset(t *testing.T) {
	if !IsSuperset([]string{"S1"}, []string{"S1", "S2"}) {
		t.Fatal("应为严格超集")
	}
	if IsSuperset([]string{"S1", "S2"}, []string{"S1", "S2"}) {
		t.Fatal("相等不是扩大")
	}
	if IsSuperset([]string{"S1", "S9"}, []string{"S1", "S2"}) {
		t.Fatal("不含旧集合不算扩大")
	}
}

func TestIsSubsetAndEqual(t *testing.T) {
	if !IsSubset([]string{"S1"}, []string{"S1", "S2"}) {
		t.Fatal("应为子集")
	}
	if !Equal([]string{"S2", "S1"}, []string{"S1", "S2"}) {
		t.Fatal("无序相等")
	}
	if Equal([]string{"S1"}, []string{"S1", "S2"}) {
		t.Fatal("长度不等不应相等")
	}
}
