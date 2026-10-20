package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/misakano7545/qoder2api-panel/internal/auth"
	"github.com/misakano7545/qoder2api-panel/internal/cosy"
)

// authState 是账号凭证的类型别名：本包的签名里统一用短名。
type authState = auth.Auth

// encodeBody / buildAuthHeaders 是 cosy 的薄包装，签名只在本包出现一次。
func encodeBody(payload []byte) []byte { return cosy.EncodeBody(payload) }

func buildAuthHeaders(body []byte, rawURL string, a *authState) (map[string]string, error) {
	return cosy.BuildAuthHeaders(body, rawURL, cosy.Identity{
		UserID:    a.UID,
		AuthToken: a.JWT(),
		Name:      a.Nickname,
		Email:     a.Email,
		MachineID: a.MachineIDValue(),
	})
}

// 上游信封固定字段（照官方客户端，不要自创）。
const (
	// upstreamSessionType 实测为 "app"（客户端形态是桌面 App，不是 CLI）。
	upstreamSessionType = "app"
	upstreamAgentID     = "agent_common"
	upstreamChatTask    = "FREE_INPUT"
)

// thinkTags 是上游可能把思考内容包在正文里的标签。
var thinkTags = [][2]string{
	{"<thinking>", "</thinking>"},
	{"<think>", "</think>"},
	{"<reasoning>", "</reasoning>"},
	{"<thought>", "</thought>"},
}

// thinkingChoice 是一次请求的思考设置。Enable 为 nil 表示「不指定」。
type thinkingChoice struct {
	Enable *bool
	Effort string
}

// contentToText 把 OpenAI content（字符串或内容块数组）压平成文本。
// 数组里的非文本块（image_url 等）跳过而不是报错。
func contentToText(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		var out strings.Builder
		for _, item := range typed {
			switch block := item.(type) {
			case string:
				out.WriteString(block)
			case map[string]any:
				if text, ok := block["text"].(string); ok {
					out.WriteString(text)
				} else if text, ok := block["content"].(string); ok {
					out.WriteString(text)
				}
			}
		}
		return out.String()
	default:
		return ""
	}
}

// imagesOf 抽出消息里的图片块（OpenAI 的 image_url 形态）。
func imagesOf(content any) []any {
	items, ok := content.([]any)
	if !ok {
		return nil
	}
	out := []any{}
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "image_url" {
			out = append(out, item)
		}
	}
	return out
}

// normalizeMessages 把 OpenAI messages 规整成上游能吃的形状：丢弃失败/中断的
// assistant 轮次及其 tool 结果、assistant 只有工具调用时补一个非空 content、
// 用户图片保持 image_url 块形态。
func normalizeMessages(messages []any) []any {
	out := []any{}
	dropped := map[string]bool{}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role == "" {
			continue
		}
		if role == "assistant" && (truthy(message["__failed"]) || truthy(message["__aborted"])) {
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, call := range calls {
					if cm, ok := call.(map[string]any); ok {
						if id, ok := cm["id"].(string); ok {
							dropped[id] = true
						}
					}
				}
			}
			continue
		}
		if role == "tool" {
			if id, _ := message["tool_call_id"].(string); dropped[id] {
				continue
			}
		}

		switch role {
		case "user":
			content := message["content"]
			images := imagesOf(content)
			if len(images) == 0 {
				out = append(out, map[string]any{"role": "user", "content": contentToText(content)})
				continue
			}
			parts := []any{}
			if text := contentToText(content); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, image := range images {
				block, _ := image.(map[string]any)
				parts = append(parts, map[string]any{"type": "image_url", "image_url": block["image_url"]})
			}
			out = append(out, map[string]any{"role": "user", "content": parts})
		case "assistant":
			text := contentToText(message["content"])
			toolCalls := normalizeToolCalls(message["tool_calls"])
			content := text
			if text == "" && len(toolCalls) > 0 {
				content = " "
			}
			mapped := map[string]any{"role": "assistant", "content": content}
			if len(toolCalls) > 0 {
				mapped["tool_calls"] = toolCalls
			}
			out = append(out, mapped)
		case "tool":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": message["tool_call_id"],
				"content":      contentToText(message["content"]),
			})
		case "system", "developer":
			out = append(out, map[string]any{"role": "system", "content": contentToText(message["content"])})
		}
	}
	return out
}

// normalizeToolCalls 把 assistant 的 tool_calls 规整成上游形态。
func normalizeToolCalls(raw any) []any {
	calls, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := []any{}
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, _ := call["function"].(map[string]any)
		arguments := "{}"
		if value, ok := function["arguments"]; ok {
			if text, ok := value.(string); ok {
				arguments = text
			} else if encoded, err := json.Marshal(value); err == nil {
				arguments = string(encoded)
			}
		}
		out = append(out, map[string]any{
			"id":   call["id"],
			"type": "function",
			"function": map[string]any{
				"name":      function["name"],
				"arguments": arguments,
			},
		})
	}
	return out
}

// normalizeTools 把 OpenAI tools 转成上游形态；没有合法工具时返回 nil。
func normalizeTools(tools any) []any {
	items, ok := tools.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	out := []any{}
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		slim := map[string]any{"name": name}
		if description, ok := function["description"]; ok {
			slim["description"] = description
		}
		if parameters, ok := function["parameters"]; ok {
			slim["parameters"] = parameters
		}
		out = append(out, map[string]any{"type": "function", "function": slim})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// declaredReasoning 取请求里表达思考档位的原始值。
func declaredReasoning(body map[string]any) (any, bool) {
	for _, key := range []string{"reasoning_effort", "reasoning", "thinking"} {
		if value, ok := body[key]; ok {
			return value, true
		}
	}
	return nil, false
}

// resolveThinking 把下游的思考强度映射成上游接受的参数。
func resolveThinking(body map[string]any, model ModelInfo) thinkingChoice {
	raw, _ := declaredReasoning(body)
	if !model.Reasoning {
		return thinkingChoice{}
	}
	if flag, ok := raw.(bool); ok && !flag {
		return thinkingChoice{}
	}
	if text, ok := raw.(string); ok {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "off", "none", "disabled":
			return thinkingChoice{}
		}
	}
	if raw == nil {
		enable := true
		return thinkingChoice{Enable: &enable}
	}
	if flag, ok := raw.(bool); ok && flag {
		enable := true
		return thinkingChoice{Enable: &enable}
	}

	asked := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
	wanted := asked
	switch asked {
	case "minimal", "min":
		wanted = "low"
	case "high", "max":
		wanted = "xhigh"
	}
	effort := wanted
	found := false
	for _, known := range model.Efforts {
		if known == wanted {
			found = true
			break
		}
	}
	if !found {
		// 请求的档位模型不支持：退回 medium（没有就第一档），而不是发一个无效值。
		effort = ""
		for _, known := range model.Efforts {
			if known == "medium" {
				effort = known
				break
			}
		}
		if effort == "" && len(model.Efforts) > 0 {
			effort = model.Efforts[0]
		}
		if effort == "" {
			effort = "medium"
		}
	}
	enable := true
	return thinkingChoice{Enable: &enable, Effort: effort}
}

// buildUpstreamBody 组装 agent_chat_generation 请求体。
func buildUpstreamBody(in map[string]any, model ModelInfo, userID string) map[string]any {
	messages := normalizeMessages(asSlice(in["messages"]))
	tools := normalizeTools(in["tools"])
	thinking := resolveThinking(in, model)

	limit := int64(MaxOutputTokens)
	if value, ok := asInt64(in["max_tokens"]); ok && value > 0 {
		limit = value
	} else if value, ok := asInt64(in["max_completion_tokens"]); ok && value > 0 {
		limit = value
	}
	if limit > MaxOutputTokens {
		limit = MaxOutputTokens
	}
	lastUser := lastUserText(messages)
	sessionSeed := firstString(in["user"], in["session_id"])

	parameters := map[string]any{
		"max_tokens": limit,
		// 实测恒为 200000（两区、各档位样本都一样），不跟随模型
		// 的 max_input_tokens（180000/1000000 都出现过，但这里始终 200000）。
		"context_length": DefaultContextWindow,
	}
	if thinking.Enable != nil {
		if *thinking.Enable {
			parameters["enable_thinking"] = true
			if thinking.Effort != "" {
				parameters["reasoning_effort"] = thinking.Effort
			}
		} else {
			parameters["enable_thinking"] = false
		}
	}
	if tools == nil {
		tools = []any{}
	}

	// 实测：request_id == chat_record_id（同一次请求），
	// request_set_id == business.id（同一轮对话集合）。
	// 旧实现把 request_id 和 chat_record_id 拆成了两个值，上游虽然容忍，
	// 但会让服务端的 request/record 关联断掉。
	requestID := randomUUIDOrEmpty()
	recordSetID := recordIDFor(model.UpstreamKey, messages, tools, limit)

	return map[string]any{
		"request_id":       requestID,
		"request_set_id":   recordSetID,
		"chat_record_id":   requestID,
		"session_id":       sessionIDFor(userID, model.UpstreamKey, sessionSeed),
		"stream":           true,
		"chat_task":        upstreamChatTask,
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"session_type":     upstreamSessionType,
		"agent_id":         upstreamAgentID,
		"task_id":          "common",
		"aliyun_user_type": "",
		// 实测 system 是 [{type:text,text:…}] 的块数组（与 messages[0] 同内容），
		// 不是空串；messages 里的 system 轮次同样必须保留。
		"system":     systemBlocks(lastUser),
		"messages":   messages,
		"tools":      tools,
		"parameters": parameters,
		"chat_context": map[string]any{
			"chatPrompt": "",
			"imageUrls":  nil,
			"extra": map[string]any{
				"context": []any{},
				"modelConfig": map[string]any{
					"key":          model.UpstreamKey,
					"is_reasoning": model.Reasoning,
				},
				"originalContent": lastUser,
			},
			"features": []any{},
			"text":     lastUser,
		},
		"model_config": slimModelConfig(model),
		// 实测 business.product = app、version = COSY 版本、id = request_set_id
		//（与顶层同一个值），并带 model_key。
		"business": map[string]any{
			"product":   cosy.BusinessProduct,
			"version":   cosy.GatewayVersion,
			"type":      cosy.BusinessType,
			"stage":     "start",
			"id":        recordSetID,
			"name":      truncate(lastUser, 40),
			"begin_at":  time.Now().UnixMilli(),
			"model_key": model.UpstreamKey,
		},
	}
}

// slimModelConfig 精简发往上游的 model_config：必须剥掉 thinking_config，
// 否则会覆盖 parameters.enable_thinking，导致「关闭思考」失效。
func slimModelConfig(model ModelInfo) map[string]any {
	// 实测字段：key/display_name/model/format/is_vl/is_reasoning/
	// api_key/url/source/max_input_tokens。thinking_config 仍然必须剥掉，
	// 否则会覆盖 parameters.enable_thinking。
	slim := map[string]any{
		"key":              model.UpstreamKey,
		"display_name":     model.Name,
		"model":            "",
		"format":           "openai",
		"api_key":          "",
		"url":              "",
		"is_vl":            model.VL,
		"is_reasoning":     model.Reasoning,
		"max_input_tokens": model.ContextWindow,
	}
	if model.Source != "" {
		slim["source"] = model.Source
	} else {
		slim["source"] = "system"
	}
	return slim
}

// systemBlocks 生成上游要的 system 块数组。
// 实测两区都是 2 个 text block（第一块是指令原文，第二块是环境附加段），
// 不是空串也不是单块。
func systemBlocks(text string) []any {
	return []any{
		map[string]any{"type": "text", "text": text},
		map[string]any{"type": "text", "text": ""},
	}
}

// lastUserText 取最后一条用户文本（上游要求带在 context 与 business 里）。
func lastUserText(messages []any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role == "user" {
			return contentToText(message["content"])
		}
	}
	return ""
}

// sessionIDFor 派生会话 id：同一账号 + 同一模型 + 同一种子得到同一个。
func sessionIDFor(userID, upstreamKey, seed string) string {
	hasher := sha256.New()
	hasher.Write([]byte("qoder-session"))
	hasher.Write([]byte{0})
	hasher.Write([]byte(userID))
	hasher.Write([]byte{0})
	hasher.Write([]byte(upstreamKey))
	base := hex.EncodeToString(hasher.Sum(nil))[:16]
	if seed != "" {
		return base + "-" + seed
	}
	return base + "-" + randomUUIDOrEmpty()
}

// recordIDFor 计算请求指纹，进 chat_record_id / request_set_id。
func recordIDFor(upstreamKey string, messages []any, tools []any, maxTokens int64) string {
	hasher := sha256.New()
	hasher.Write([]byte("qoder-record"))
	hasher.Write([]byte{0})
	hasher.Write([]byte(upstreamKey))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role != "" {
			hasher.Write([]byte{0})
			hasher.Write([]byte(role))
		}
		if content, ok := message["content"]; ok && content != nil {
			hasher.Write([]byte{0})
			if text, ok := content.(string); ok {
				hasher.Write([]byte(text))
			} else if encoded, err := json.Marshal(content); err == nil {
				hasher.Write(encoded)
			}
		}
	}
	if tools != nil {
		hasher.Write([]byte{0})
		if encoded, err := json.Marshal(tools); err == nil {
			hasher.Write(encoded)
		}
	}
	hasher.Write([]byte{0})
	hasher.Write([]byte(fmt.Sprintf("mt=%d", maxTokens)))
	return hex.EncodeToString(hasher.Sum(nil))[:16]
}

func randomUUIDOrEmpty() string {
	value, err := cosy.RandomUUID()
	if err != nil {
		return ""
	}
	return value
}

// ---- 小工具 ----

// truthy 判定上游 JSON 里的布尔值。
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0
	case int64:
		return typed != 0
	case json.Number:
		f, err := typed.Float64()
		return err == nil && f != 0
	case string:
		return typed != ""
	case []any, map[string]any:
		return true
	}
	return false
}

// asSlice 把 JSON 数组归一成 []any。
func asSlice(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return nil
}

// asInt64 把数字/数字字符串归一成 int64。
func asInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return n, true
		}
	case string:
		var out int64
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &out); err == nil {
			return out, true
		}
	}
	return 0, false
}

// firstString 取第一个非空字符串。
func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}
