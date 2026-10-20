package server

import (
	"encoding/json"
	"testing"
)

// reasoning_effort 在上游是空转参数（实测对 reasoning_tokens 无影响），真正生效的
// 只有 extra_body.thinking。这里守住翻译：客户端说「别思考」时，出站必须带
// extra_body.thinking.type=disabled，否则客户端以为关了、实际每轮还在烧 reasoning。
func TestApplyThinking(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 期望的 extra_body.thinking.type；空 = 不该注入
	}{
		{"none", `{"reasoning_effort":"none"}`, "disabled"},
		{"minimal", `{"reasoning_effort":"minimal"}`, "enabled"},
		{"high", `{"reasoning_effort":"high"}`, "enabled"},
		{"没表态", `{}`, ""},
		{"已显式给 extra_body.thinking", `{"extra_body":{"thinking":{"type":"disabled"}}}`, "disabled"},
		{"顶层 thinking", `{"thinking":{"type":"disabled"}}`, "disabled"},
		{"非法 JSON", `not json`, ""},
	}
	for _, c := range cases {
		out := applyThinking([]byte(c.in))
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			if c.want == "" && c.name == "非法 JSON" {
				continue // 原样返回，不是 JSON 也符合契约
			}
			t.Fatalf("%s: 输出不是 JSON: %v", c.name, err)
		}
		got := ""
		if eb, ok := obj["extra_body"].(map[string]any); ok {
			if th, ok := eb["thinking"].(map[string]any); ok {
				got, _ = th["type"].(string)
			}
		}
		if got != c.want {
			t.Errorf("%s: extra_body.thinking.type = %q，期望 %q（输出 %s）", c.name, got, c.want, out)
		}
	}
}

// 客户端已显式给 extra_body.thinking 时不能被我们覆盖。
func TestApplyThinkingRespectsExplicit(t *testing.T) {
	in := `{"reasoning_effort":"none","extra_body":{"thinking":{"type":"enabled"}}}`
	out := applyThinking([]byte(in))
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	eb := obj["extra_body"].(map[string]any)
	th := eb["thinking"].(map[string]any)
	if th["type"] != "enabled" {
		t.Fatalf("显式 thinking 被覆盖成 %v", th["type"])
	}
}
