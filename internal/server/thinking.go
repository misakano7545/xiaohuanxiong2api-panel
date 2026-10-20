// thinking.go 思考开关口径：把小浣熊真正认的 `extra_body.thinking` 补进请求体。
//
// 上游实测（2026-10-21，判据是服务端上报的 reasoning_tokens）：
//
//	不传任何字段                → 31 reasoning tokens（默认开思考）
//	extra_body.thinking=disabled → 0（真关）
//	reasoning_effort=none        → 31（**接受但无效**）
//
// 也就是说 OpenAI 那套 `reasoning_effort` 对小浣熊是个空转参数：客户端以为关掉了
// 思考，实际每轮还在烧 reasoning token（既慢又费）。这里把常见写法翻译成上游
// 真正认的那个对象，两个字段并存也无害（上游忽略 reasoning_effort）。
package server

import "encoding/json"

// thinkingTypeOf 把各种「档位」写法归一成 upstream 认的 enabled/disabled。
//
// 返回空串表示「不要注入」：没表态时保持上游默认（默认就是开），少发一个字段更稳，
// 也不去猜调用方的意图。
func thinkingTypeOf(effort string) string {
	switch lower(effort) {
	case "none", "off", "disabled", "disable", "no", "0", "false":
		return "disabled"
	case "minimal", "low", "medium", "high", "on", "enabled", "adaptive", "auto", "1", "true":
		return "enabled"
	}
	return ""
}

// lower 只做 ASCII 小写（档位字面量都是 ASCII，不值得为它引 unicode 包）。
func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// applyThinking 按 reasoning_effort / 顶层 thinking 补 `extra_body.thinking`。
//
// 只在调用方明确表态时注入；已经是 extra_body.thinking 的请求原样放过（调用方
// 比我们更清楚要什么）。解析失败一律原样返回——这类改写不该让请求失败。
func applyThinking(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	// 已经带了 extra_body.thinking：尊重调用方，不覆盖。
	if eb, ok := obj["extra_body"].(map[string]any); ok {
		if _, has := eb["thinking"]; has {
			return body
		}
	}
	effort := ""
	if v, ok := obj["reasoning_effort"].(string); ok {
		effort = v
	} else if th, ok := obj["thinking"].(map[string]any); ok {
		// Anthropic 风格顶层 thinking（/v1/messages 转换后也可能落在这里）。
		if t, ok := th["type"].(string); ok {
			effort = t
		}
	}
	want := thinkingTypeOf(effort)
	if want == "" {
		return body
	}
	eb, _ := obj["extra_body"].(map[string]any)
	if eb == nil {
		eb = map[string]any{}
	}
	eb["thinking"] = map[string]any{"type": want}
	obj["extra_body"] = eb
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
