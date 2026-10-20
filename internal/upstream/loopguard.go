package upstream

import "strings"

// reasoningGuard 检测流式思考里的机械复读（「发癫」）。判据移植自 wild-work 的
// loopguard（阈值对齐其在真实会话上标定的值），只保留两条最不容易误杀、且正常
// 思考绝不会出现的形态：
//
//	1. 同一行连续重复 ≥8 次，且行长 ≥12 字符；
//	2. 行内周期复读：末尾存在长度 p∈[2,40] 的单元，连续重复 ≥16 次且累计 ≥160 字符，
//	   单元须含字母或汉字（纯符号/纯数字是正常结构重复，不算）。
//
// 立场：宁可漏报，不可误杀——中断一次正在跑的生成是有代价的。
type reasoningGuard struct {
	tail    []rune // 只留够判定 2 的窗口
	line    []rune // 当前未结束的行
	last    string // 上一整行（判定 1）
	sameRun int
}

const (
	guardTailKeep      = 400
	guardInlineRepeats = 16
	guardInlineChars   = 160
	guardRepeatLineRun = 8
	guardRepeatLineMin = 12
	guardUnitMax       = 40
)

// reasoningLoopHit 是一次命中。
type reasoningLoopHit struct {
	Kind   string
	Detail string
}

// Push 喂入一段推理增量；命中返回非 nil。
func (g *reasoningGuard) Push(text string) *reasoningLoopHit {
	for _, r := range text {
		g.tail = append(g.tail, r)
		if r != '\n' {
			g.line = append(g.line, r)
			continue
		}
		if hit := g.checkLine(string(g.line)); hit != nil {
			return hit
		}
		g.line = g.line[:0]
	}
	if len(g.tail) > guardTailKeep {
		g.tail = g.tail[len(g.tail)-guardTailKeep:]
	}
	return g.checkInline()
}

// checkLine 判据 1：同一行连续重复。
func (g *reasoningGuard) checkLine(raw string) *reasoningLoopHit {
	line := strings.TrimSpace(raw)
	// 纯符号/纯数字的行（长分隔线、括号、序号）重复是正常结构，不算复读。
	if len([]rune(line)) < guardRepeatLineMin || !hasWordChar([]rune(line)) {
		g.sameRun, g.last = 0, ""
		return nil
	}
	if line == g.last {
		g.sameRun++
	} else {
		g.last, g.sameRun = line, 1
	}
	if g.sameRun >= guardRepeatLineRun {
		return &reasoningLoopHit{Kind: "repeat_line", Detail: clip(line, 60)}
	}
	return nil
}

// checkInline 判据 2：行内周期复读。
func (g *reasoningGuard) checkInline() *reasoningLoopHit {
	tail := g.tail
	n := len(tail)
	if n < guardInlineChars {
		return nil
	}
	for p := 2; p <= guardUnitMax; p++ {
		unit := tail[n-p:]
		if !hasWordChar(unit) || strings.ContainsRune(string(unit), '\n') {
			continue
		}
		repeats := 0
		for off := n; off-p >= 0; off -= p {
			if !equalRunes(tail[off-p:off], unit) {
				break
			}
			repeats++
		}
		if repeats >= guardInlineRepeats && repeats*p >= guardInlineChars {
			return &reasoningLoopHit{Kind: "repeat_inline", Detail: clip(string(unit), 40)}
		}
	}
	return nil
}

func hasWordChar(runes []rune) bool {
	for _, r := range runes {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 0x2E80 {
			return true
		}
	}
	return false
}

func equalRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
