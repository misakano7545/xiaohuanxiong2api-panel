package server

// 观测面：声明了 tools 的回合要把「声明数 / 实际调用数」落进请求记录。
// 这两个数一起才判得出「模型停摆」——声明了工具、上游却正常 stop 且零 tool_calls，
// 客户端看到的就是一次什么都没做的回合。反例（没声明 tools 的纯聊天）不得计入，
// 否则分母是错的、比率没有意义。

import (
	"net/http"
	"strings"
	"testing"
)

const toolsBody = `"tools":[{"type":"function","function":{"name":"exec_command","parameters":{"type":"object"}}}]`

func TestRequestLogRecordsToolsDeclared(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		calls int
	}{
		// 假上游的流里固定有一帧 tool_calls，所以流式看的是「客户端收到过工具帧」= 1。
		{"流式：收到过工具帧", `{"model":"raccoon-chat-ml-5-5","stream":true,"messages":[{"role":"user","content":"列目录"}],` + toolsBody + `}`, 1},
		// 假上游的非流式响应没有 tool_calls：声明了却零调用，正是要记下的停摆形态。
		{"非流式：声明了 tools 但零工具调用收尾", `{"model":"raccoon-chat-ml-5-5","messages":[{"role":"user","content":"列目录"}],` + toolsBody + `}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newFakeHandler(t)
			if rec := post(t, h, "/v1/chat/completions", tc.body); rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			ev := h.cfg.RequestLog.Snapshot().Recent
			if len(ev) != 1 {
				t.Fatalf("recent=%+v", ev)
			}
			if ev[0].ToolsDeclared != 1 || ev[0].ToolCalls != tc.calls {
				t.Fatalf("tools_declared=%d tool_calls=%d, want 1/%d",
					ev[0].ToolsDeclared, ev[0].ToolCalls, tc.calls)
			}
		})
	}
}

// tools 形态不合法（对象而非数组）时 json 解码只会跳过该字段，stream/model 照样认得出；
// 这个字段只用来数数，不能因为它把一条流式请求降级成非流式。声明数按 0 算。
func TestToolsMalformedKeepsRouting(t *testing.T) {
	h, _ := newFakeHandler(t)
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"raccoon-chat-ml-5-5","stream":true,"tools":{},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "chat.completion.chunk") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ev := h.cfg.RequestLog.Snapshot().Recent; len(ev) != 1 || ev[0].ToolsDeclared != 0 {
		t.Fatalf("recent=%+v", ev)
	}
}

// 没声明 tools 的纯聊天不进分母：两个字段都保持零值（JSON 里被 omitempty 省掉）。
func TestRequestLogIgnoresPlainChat(t *testing.T) {
	h, _ := newFakeHandler(t)
	if rec := post(t, h, "/v1/chat/completions",
		`{"model":"raccoon-chat-ml-5-5","messages":[{"role":"user","content":"在吗"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	ev := h.cfg.RequestLog.Snapshot().Recent
	if len(ev) != 1 || ev[0].ToolsDeclared != 0 || ev[0].ToolCalls != 0 {
		t.Fatalf("recent=%+v", ev)
	}
}

// 非流式的计数要认两种组包形态：Aggregate 自己组的是 []any，直解上游 JSON 也可能
// 是 []map[string]any。只认一种会让这条路径恒记 0（线上冒烟就是这么发现的）。
func TestToolCallCountBothShapes(t *testing.T) {
	resp := func(tcs any) map[string]any {
		return map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"tool_calls": tcs},
		}}}
	}
	if n := toolCallCount(resp([]any{map[string]any{"id": "a"}, map[string]any{"id": "b"}})); n != 2 {
		t.Fatalf("[]any: n=%d, want 2", n)
	}
	if n := toolCallCount(resp([]map[string]any{{"id": "a"}})); n != 1 {
		t.Fatalf("[]map[string]any: n=%d, want 1", n)
	}
	if n := toolCallCount(map[string]any{}); n != 0 {
		t.Fatalf("空响应: n=%d, want 0", n)
	}
}
