// Package match 负责把新异物报告关联到已有未闭环事件。
// 关联依据:位置相近(同区段或相邻区段) + 影像特征相似(感知哈希汉明距离)。
package match

import (
	"encoding/hex"
	"sort"
)

// MaxHamming 是判定两次影像为同一异物的最大汉明距离(64 位感知哈希)。
const MaxHamming = 10

// Candidate 是一个可关联的未闭环事件。
type Candidate struct {
	EventID        string
	SeedSegments   []string
	ImageSignature string
}

// Hamming 计算两个 64 位感知哈希(hex 编码)的汉明距离。
// 任一签名缺失或非法时返回 -1,表示"无法比较",由调用方降级为仅位置匹配。
func Hamming(sigA, sigB string) int {
	if sigA == "" || sigB == "" {
		return -1
	}
	a, errA := hex.DecodeString(sigA)
	b, errB := hex.DecodeString(sigB)
	if errA != nil || errB != nil || len(a) != len(b) || len(a) == 0 {
		return -1
	}
	d := 0
	for i := range a {
		x := a[i] ^ b[i]
		for x != 0 {
			d += int(x & 1)
			x >>= 1
		}
	}
	return d
}

// Score 评估一条报告与候选事件的匹配度;ok=false 表示不可关联。
//   - 位置必须相同或相邻(adjacent 由调用方依据拓扑给出);
//   - 双方都有影像签名时,汉明距离必须 ≤ MaxHamming;
//   - 任一签名缺失时降级为仅位置匹配,得分较低但仍可关联。
func Score(reportSeg, reportSig string, c Candidate, adjacent bool) (score int, ok bool) {
	sameSeg := false
	for _, s := range c.SeedSegments {
		if s == reportSeg {
			sameSeg = true
			break
		}
	}
	if !sameSeg && !adjacent {
		return 0, false
	}
	score = 2 // 相邻
	if sameSeg {
		score = 4 // 同区段
	}
	switch d := Hamming(reportSig, c.ImageSignature); {
	case d < 0:
		// 无签名可比,仅位置匹配
	case d <= MaxHamming:
		score += 10 - d/2 // 影像相似,距离越近得分越高
	default:
		return 0, false // 影像明显不同,即使位置相近也不并案
	}
	return score, true
}

// Best 在候选中选出得分最高的关联事件;无可关联时返回 ok=false(应新建事件)。
func Best(reportSeg, reportSig string, cands []Candidate, isAdjacent func(eventSegs []string, seg string) bool) (bestID string, ok bool) {
	type hit struct {
		id    string
		score int
	}
	var hits []hit
	for _, c := range cands {
		s, ok := Score(reportSeg, reportSig, c, isAdjacent(c.SeedSegments, reportSeg))
		if ok {
			hits = append(hits, hit{c.EventID, s})
		}
	}
	if len(hits) == 0 {
		return "", false
	}
	// 得分降序、ID 升序,保证并列时结果确定
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].id < hits[j].id
	})
	return hits[0].id, true
}
