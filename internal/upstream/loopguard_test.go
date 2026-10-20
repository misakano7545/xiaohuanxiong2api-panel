package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 判据 1：同一行连续重复 ≥8 次。
func TestReasoningGuardRepeatLine(t *testing.T) {
	g := &reasoningGuard{}
	line := "接下来我要仔细检查这个函数是否正确。\n"
	var hit *reasoningLoopHit
	for i := 0; i < 10 && hit == nil; i++ {
		hit = g.Push(line)
	}
	if hit == nil || hit.Kind != "repeat_line" {
		t.Fatalf("应命中 repeat_line，得到 %+v", hit)
	}
}

// 判据 2：行内片段周期复读。
func TestReasoningGuardRepeatInline(t *testing.T) {
	g := &reasoningGuard{}
	unit := "好我再看一遍"
	var hit *reasoningLoopHit
	for i := 0; i < 40 && hit == nil; i++ {
		hit = g.Push(unit + "，")
	}
	if hit == nil || hit.Kind != "repeat_inline" {
		t.Fatalf("应命中 repeat_inline，得到 %+v", hit)
	}
}

// 正常思考与正常结构重复（长分隔线、括号）不能误杀。
func TestReasoningGuardNoFalsePositive(t *testing.T) {
	for _, text := range []string{
		"第一步，确认参数类型是 string。\n第二步，找到调用点。\n第三步，改掉它。\n",
		strings.Repeat("────────────────\n", 20), // 长分隔线：纯符号，属正常结构
		strings.Repeat("}\n", 40), // 代码闭括号：正常
		"```go\nfunc a() {}\nfunc b() {}\nfunc c() {}\n```\n",
	} {
		g := &reasoningGuard{}
		var hit *reasoningLoopHit
		for _, chunk := range splitChunks(text, 7) { // 分片喂，模拟流式
			if hit = g.Push(chunk); hit != nil {
				break
			}
		}
		if hit != nil {
			t.Errorf("误杀正常内容（%s）：%+v", clip(text, 20), hit)
		}
	}
}

// 流式到一半命中：客户端要收到 upstream_reasoning_loop 错误帧并结束。
func TestStreamAbortsOnReasoningLoop(t *testing.T) {
	var b strings.Builder
	push := func(delta map[string]any) {
		chunk, _ := json.Marshal(map[string]any{"id": "c1", "model": "Auto",
			"choices": []any{map[string]any{"index": 0, "delta": delta}}})
		env, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": string(chunk)})
		b.WriteString("data: " + string(env) + "\n\n")
	}
	push(map[string]any{"content": "开始处理"})
	for i := 0; i < 12; i++ {
		push(map[string]any{"reasoning_content": "我得再看一遍这段代码是否正确。\n"})
	}
	rec := httptest.NewRecorder()
	var got *StreamError
	_, err := StreamWithError(rec, strings.NewReader(b.String()), func(se *StreamError) { got = se })
	if err != nil {
		t.Fatalf("流式不应把复读当调用失败：%v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "upstream_reasoning_loop") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("应下发 upstream_reasoning_loop 并收尾：%s", clip(out, 300))
	}
	if got == nil || got.Kind() != ErrReasoningLoop {
		t.Errorf("onErr 应收到 ErrReasoningLoop，得到 %+v", got)
	}
}

func splitChunks(s string, n int) []string {
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}
