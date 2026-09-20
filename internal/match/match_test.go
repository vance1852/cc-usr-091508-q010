package match

import "testing"

func TestHamming(t *testing.T) {
	if d := Hamming("ffffffffffffffff", "ffffffffffffffff"); d != 0 {
		t.Fatalf("相同签名距离应为 0, 得 %d", d)
	}
	// 1 位差异
	if d := Hamming("0000000000000000", "0000000000000001"); d != 1 {
		t.Fatalf("1 位差异应为 1, 得 %d", d)
	}
	// 任一缺失 → 不可比较
	if d := Hamming("", "ffffffffffffffff"); d != -1 {
		t.Fatalf("缺失签名应返回 -1, 得 %d", d)
	}
	// 非法 hex → 不可比较
	if d := Hamming("zz", "ff"); d != -1 {
		t.Fatalf("非法签名应返回 -1, 得 %d", d)
	}
}

func TestScoreRequiresLocationProximity(t *testing.T) {
	c := Candidate{EventID: "e1", SeedSegments: []string{"S3"}, ImageSignature: "ffffffffffffffff"}
	if _, ok := Score("S9", "ffffffffffffffff", c, false); ok {
		t.Fatal("位置不相近不可关联")
	}
}

func TestScoreRejectsDissimilarImage(t *testing.T) {
	c := Candidate{EventID: "e1", SeedSegments: []string{"S3"}, ImageSignature: "0000000000000000"}
	// 64 位全不同,远超阈值
	if _, ok := Score("S3", "ffffffffffffffff", c, false); ok {
		t.Fatal("影像明显不同不应并案")
	}
}

func TestScoreDegradesToLocationOnly(t *testing.T) {
	c := Candidate{EventID: "e1", SeedSegments: []string{"S3"}}
	s, ok := Score("S3", "", c, false)
	if !ok {
		t.Fatal("无签名时应降级为位置匹配")
	}
	if s != 4 {
		t.Fatalf("同区段无签名得分应为 4, 得 %d", s)
	}
}

func TestBestPicksHighestScore(t *testing.T) {
	cands := []Candidate{
		{EventID: "e-far", SeedSegments: []string{"S5"}, ImageSignature: "ffffffffffffffff"},
		{EventID: "e-near", SeedSegments: []string{"S3"}, ImageSignature: "fffffffffffffffe"}, // 1 位差
	}
	adjacent := func(segs []string, seg string) bool {
		for _, s := range segs {
			if s == "S5" && seg == "S3" {
				return true
			}
		}
		return false
	}
	id, ok := Best("S3", "ffffffffffffffff", cands, adjacent)
	if !ok || id != "e-near" {
		t.Fatalf("应选中影像更近的 e-near, 得 %v %v", id, ok)
	}
}

func TestBestEmptyWhenNoCandidate(t *testing.T) {
	if _, ok := Best("S1", "", nil, nil); ok {
		t.Fatal("无候选不应关联")
	}
}
