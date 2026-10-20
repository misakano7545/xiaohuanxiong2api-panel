package upstream

import "testing"

// 本文件把「国际版 / 国内版双区实测」的载荷不变量钉成回归测试。
// 样本来源：Qoder 0.4.8 客户端 MITM 抓包（两区各一条 agent_chat_generation）。
//
// 这些不变量此前都被实现猜错过一次：
//   - request_id 与 chat_record_id 拆成两个值（实测同值）
//   - context_length 跟着模型走（实测恒 200000）
//   - system 是空串 / 单块（实测 2 个 text block）
//   - 顶层多出 code_language / chat_prompt / image_urls（实测不存在）

func TestWireShapeInvariants(t *testing.T) {
	model := ModelInfo{
		ID:            "DeepSeek-Flash",
		Name:          "DeepSeek-Flash",
		UpstreamKey:   "dfmodel",
		Reasoning:     true,
		VL:            true,
		Source:        "system",
		ContextWindow: 180000,
	}
	in := map[string]any{
		"model": "dfmodel",
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "1+1=?"},
		},
	}
	body := buildUpstreamBody(in, model, "u-1")

	// request_id == chat_record_id：同一次请求的两个字段必须一致。
	if body["request_id"] != body["chat_record_id"] {
		t.Errorf("request_id(%v) 与 chat_record_id(%v) 实测应同值",
			body["request_id"], body["chat_record_id"])
	}
	// request_set_id == business.id：同一轮对话集合。
	business := body["business"].(map[string]any)
	if body["request_set_id"] != business["id"] {
		t.Errorf("request_set_id(%v) 与 business.id(%v) 实测应同值",
			body["request_set_id"], business["id"])
	}
	// request_set_id 不能等于 request_id（否则「集合」语义退化）。
	if body["request_set_id"] == body["request_id"] {
		t.Errorf("request_set_id 与 request_id 不应该相同")
	}

	// context_length 恒 200000，不跟随 model.ContextWindow(180000)。
	params := body["parameters"].(map[string]any)
	if got, ok := asInt64(params["context_length"]); !ok || got != int64(DefaultContextWindow) {
		t.Errorf("context_length = %v, want %d", params["context_length"], DefaultContextWindow)
	}
	if DefaultContextWindow != 200_000 {
		t.Errorf("DefaultContextWindow = %d, 实测为 200000", DefaultContextWindow)
	}

	// system 是 2 个 text block。
	blocks, ok := body["system"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("system 应为 2 个 text block，得到 %v", body["system"])
	}
	for i, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "text" {
			t.Errorf("system[%d] 不是 text block: %v", i, raw)
		}
	}

	// 顶层不能有实测不存在的字段。
	for _, banned := range []string{"code_language", "chat_prompt", "image_urls"} {
		if _, exists := body[banned]; exists {
			t.Errorf("顶层不该有 %q（实测上游请求里没有这个键）", banned)
		}
	}

	// business 三件套 + model_key。
	if business["product"] != "app" || business["type"] != "agent" || business["model_key"] != "dfmodel" {
		t.Errorf("business = %v", business)
	}
	if business["version"] != "1.1.69" {
		t.Errorf("business.version = %v, 实测为 1.1.69", business["version"])
	}

	// session_type 实测为 app。
	if body["session_type"] != "app" {
		t.Errorf("session_type = %v, 实测为 app", body["session_type"])
	}
}
