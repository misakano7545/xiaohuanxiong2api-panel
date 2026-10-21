package server

import (
	"encoding/json"
	"testing"
)

// 思考 token 算在 max_tokens 里：客户端给 32 时 10/20 个模型正文回空（实测 2026-10-21），
// 所以出站必须抬到下限；已经给够的、没给的、非数字的一律不动。
func TestLiftMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64 // 期望的出站值；0 = 字段不该被改
	}{
		{"小额度被抬到下限", `{"max_tokens":32}`, minOutputTokens},
		{"64 也抬", `{"max_tokens":64}`, minOutputTokens},
		{"给够了不动", `{"max_tokens":4096}`, 4096},
		{"等于下限不动", `{"max_tokens":512}`, minOutputTokens},
		{"没给就不发明", `{}`, 0},
		{"0 当没给", `{"max_tokens":0}`, 0},
		{"responses 的字段名也认", `{"max_output_tokens":16}`, minOutputTokens},
		{"chat 的 max_completion_tokens 也认", `{"max_completion_tokens":16}`, minOutputTokens},
	}
	for _, c := range cases {
		var got map[string]any
		if err := json.Unmarshal(liftMaxTokens([]byte(c.in)), &got); err != nil {
			t.Fatalf("%s: 输出不是 JSON: %v", c.name, err)
		}
		var v float64
		for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
			if f, ok := got[k].(float64); ok {
				v = f
			}
		}
		if v != c.want {
			t.Errorf("%s: 出站额度 = %v，期望 %v（输出 %v）", c.name, v, c.want, got)
		}
	}
}

// 非 JSON 体原样返回：上游客户端自己会报错，这里不该多一层改动。
func TestLiftMaxTokensKeepsInvalidBody(t *testing.T) {
	in := "not json"
	if out := liftMaxTokens([]byte(in)); string(out) != in {
		t.Errorf("非法 JSON 被改动了: %s", out)
	}
}
