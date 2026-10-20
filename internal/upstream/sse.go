package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// StreamError 是流内业务错误（信封 statusCodeValue != 200）。
// 名字沿用旧面板的错误类型形状：调用方只看 Kind()/Error()。
type StreamError struct {
	KindValue ErrKind
	Status    int
	Msg       string
	// RetryAfterSecs 是上游建议的重试间隔（排队时才有），供调用方退避重发。
	RetryAfterSecs int64
	// ServiceUnavailable = serviceAvailable:false：此刻排不上，等也没用。
	ServiceUnavailable bool
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("upstream %s (status %d): %s", e.KindValue, e.Status, e.Msg)
}

// Kind 返回错误分类（供 pool 记账）。
func (e *StreamError) Kind() ErrKind { return e.KindValue }

// sseEventKind 是一行 SSE 的语义分类。
type sseEventKind int

const (
	sseSkip sseEventKind = iota
	sseDone
	sseChunk
	sseError
)

// sseEvent 是一条 SSE 事件的解析结果。
type sseEvent struct {
	Kind    sseEventKind
	Chunk   map[string]any // Kind==sseChunk：拆掉信封后的内层 OpenAI chunk
	Status  int
	Class   ErrKind
	Raw     string
	Message string
	// RetryAfterSecs 是信封里给的重试间隔（排队态），0 = 没给。
	RetryAfterSecs int64
	// ServiceUnavailable = serviceAvailable:false。
	ServiceUnavailable bool
	// replay 是这条帧的原始字节（peekFirstFrame 用，好把已读部分放回流里）。
	replay []byte
}

// lineBuffer 按字节缓冲 SSE 行：只在完整行上解码，避免把多字节字符切坏。
type lineBuffer struct{ tail []byte }

// push 吃一段上游字节，吐出其中完整的 data: 行（已去掉 data: 前缀）。
func (b *lineBuffer) push(chunk []byte) []string {
	b.tail = append(b.tail, chunk...)
	out := []string{}
	start := 0
	for i := 0; i < len(b.tail); i++ {
		if b.tail[i] != '\n' {
			continue
		}
		line := strings.TrimSuffix(string(b.tail[start:i]), "\r")
		if strings.HasPrefix(line, "data:") {
			out = append(out, strings.TrimPrefix(line, "data:"))
		}
		start = i + 1
	}
	b.tail = b.tail[start:]
	return out
}

// finish 在流结束时吐出没有以换行收尾的最后一行。
func (b *lineBuffer) finish() []string {
	if len(b.tail) == 0 {
		return nil
	}
	line := strings.TrimSuffix(string(b.tail), "\r")
	b.tail = nil
	if strings.HasPrefix(line, "data:") {
		data := strings.TrimPrefix(line, "data:")
		if strings.TrimSpace(data) != "" {
			return []string{data}
		}
	}
	return nil
}

// parseSSELine 解析一行 data: 之后的内容：
// 外层是 {"statusCodeValue":200,"body":"<内层 chunk JSON>"}，内层才是 OpenAI chunk。
func parseSSELine(data string) sseEvent {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return sseEvent{Kind: sseSkip}
	}
	if trimmed == "[DONE]" {
		return sseEvent{Kind: sseDone}
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return sseEvent{Kind: sseSkip}
	}
	if status, ok := intOf(envelope["statusCodeValue"]); ok && status != 200 {
		raw := ""
		switch body := envelope["body"].(type) {
		case string:
			raw = body
		case nil:
		default:
			if encoded, err := json.Marshal(body); err == nil {
				raw = string(encoded)
			}
		}
		class := classifyErrorText(status, raw)
		return sseEvent{Kind: sseError, Status: status, Class: class, Raw: truncate(raw, 500),
			Message: messageOf(class), RetryAfterSecs: retryAfterOf(raw), ServiceUnavailable: queueUnavailable(raw)}
	}
	inner, ok := envelope["body"]
	if !ok {
		return sseEvent{Kind: sseSkip}
	}
	if text, ok := inner.(string); ok {
		if text == "[DONE]" {
			return sseEvent{Kind: sseDone}
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(text), &chunk); err != nil {
			return sseEvent{Kind: sseSkip}
		}
		return sseEvent{Kind: sseChunk, Chunk: chunk}
	}
	if inner == nil {
		return sseEvent{Kind: sseSkip}
	}
	if chunk, ok := inner.(map[string]any); ok {
		return sseEvent{Kind: sseChunk, Chunk: chunk}
	}
	return sseEvent{Kind: sseSkip}
}

// classifyErrorText 是信封内业务的分类：先按码/关键词判额度，再按传输层状态。
func classifyErrorText(status int, raw string) ErrKind {
	lower := strings.ToLower(raw)
	if messageHasQueue(raw) {
		return ErrBusy
	}
	for _, code := range []string{authCode} {
		if rawHasCode(raw, code) {
			return ErrSessionDead
		}
	}
	for _, code := range quotaCodes() {
		if rawHasCode(raw, code) {
			return ErrPlanLimit
		}
	}
	for _, signal := range []string{"pricingurl", "insufficient", "no_quota", "quota_exceed", "exceed_quota", "subscription", "upgrade"} {
		if strings.Contains(lower, signal) {
			return ErrPlanLimit
		}
	}
	if status == 429 || strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many") {
		return ErrSoftRate
	}
	if status == 401 {
		return ErrSessionDead
	}
	if status == 403 {
		return ErrClient
	}
	if status >= 500 {
		return ErrServer
	}
	return ErrNone
}

func messageOf(kind ErrKind) string {
	switch kind {
	case ErrPlanLimit:
		return "当前账号额度不足或套餐不支持该模型"
	case ErrSoftRate:
		return "请求过于频繁，请稍后重试"
	case ErrSessionDead:
		return "登录态已失效，请重新登录"
	case ErrBusy:
		return "上游模型排队中（模型暂不可服务）：这不是登录态或额度问题"
	case ErrServer:
		return "上游服务异常"
	}
	return "上游拒绝了本次请求"
}

// messageHasQueue 识别排队态（业务码 10605 或其语义等价字段）。
func messageHasQueue(raw string) bool {
	if rawHasCode(raw, queueCode) {
		return true
	}
	lower := strings.ToLower(raw)
	return strings.Contains(lower, "isqueued") || strings.Contains(lower, `"queuetype"`)
}

// rawHasCode 是裸文本兜底：正文不是 JSON 时也能认出业务码。
func rawHasCode(raw, code string) bool {
	flat := strings.ReplaceAll(raw, "\\", "")
	return strings.Contains(flat, `"code":"`+code+`"`) ||
		strings.Contains(flat, `"code":`+code) ||
		strings.Contains(flat, `"code": "`+code+`"`)
}

// ---------------------------------------------------------------------------
// 累积器（流式与非流式共用的语义层）

type toolState struct {
	id        string
	name      string
	arguments strings.Builder
}

// accumulator 把内层 chunk 累积成最终的非流式响应。
type accumulator struct {
	id        string
	model     string
	created   int64
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int64]*toolState
	finish    string
	usage     map[string]any
	sawAny    bool
}

func newAccumulator() *accumulator {
	return &accumulator{id: newResponseID(), created: time.Now().Unix(), tools: map[int64]*toolState{}}
}

// add 吃掉一个内层 chunk，返回需要转发给客户端的增量。
func (a *accumulator) add(chunk map[string]any) []delta {
	if id, ok := chunk["id"].(string); ok && id != "" {
		a.id = id
	}
	if model, ok := chunk["model"].(string); ok && model != "" {
		a.model = model
	}
	if created, ok := intOf(chunk["created"]); ok && created > 0 {
		a.created = int64(created)
	}
	if usage, ok := chunk["usage"].(map[string]any); ok {
		a.usage = usage
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil
	}
	if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
		a.finish = finish
	}
	d, _ := choice["delta"].(map[string]any)
	if d == nil {
		return nil
	}
	out := []delta{}
	if reasoning, ok := d["reasoning_content"].(string); ok && reasoning != "" {
		cleaned := stripThinkingTags(reasoning)
		if cleaned != "" {
			a.reasoning.WriteString(cleaned)
			out = append(out, delta{kind: deltaReasoning, text: cleaned})
			a.sawAny = true
		}
	}
	if content, ok := d["content"].(string); ok && content != "" {
		a.content.WriteString(content)
		out = append(out, delta{kind: deltaContent, text: content})
		a.sawAny = true
	}
	if calls, ok := d["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			index, _ := asInt64(call["index"])
			state := a.tools[index]
			if state == nil {
				state = &toolState{}
				a.tools[index] = state
			}
			id, _ := call["id"].(string)
			if id != "" {
				state.id = id
			}
			function, _ := call["function"].(map[string]any)
			name, _ := function["name"].(string)
			if name != "" {
				state.name = name
			}
			args, _ := function["arguments"].(string)
			state.arguments.WriteString(args)
			out = append(out, delta{kind: deltaToolCall, index: index, id: id, name: name, arguments: args})
			a.sawAny = true
		}
	}
	return out
}

// result 组装非流式 chat.completion。
func (a *accumulator) result() map[string]any {
	message := map[string]any{"role": "assistant", "content": a.content.String()}
	if a.reasoning.Len() > 0 {
		message["reasoning_content"] = a.reasoning.String()
	}
	if len(a.tools) > 0 {
		indexes := make([]int64, 0, len(a.tools))
		for index := range a.tools {
			indexes = append(indexes, index)
		}
		sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
		calls := make([]any, 0, len(indexes))
		for _, index := range indexes {
			state := a.tools[index]
			args := state.arguments.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   state.id,
				"type": "function",
				"function": map[string]any{
					"name":      state.name,
					"arguments": args,
				},
			})
		}
		message["tool_calls"] = calls
	}
	finish := a.finish
	if len(a.tools) > 0 {
		finish = "tool_calls"
	}
	if finish == "" {
		finish = "stop"
	}
	usage := a.usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	return map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
}

// delta 是一条要写给客户端的增量。
type delta struct {
	kind      int
	text      string
	index     int64
	id        string
	name      string
	arguments string
}

const (
	deltaContent = iota
	deltaReasoning
	deltaToolCall
)

// ---------------------------------------------------------------------------
// 非流式

// Aggregate 把上游 SSE 汇聚成一个 chat.completion 对象。
func Aggregate(r io.Reader) (map[string]any, error) {
	acc := newAccumulator()
	buf := &lineBuffer{}
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			for _, data := range buf.push(raw) {
				event := parseSSELine(data)
				switch event.Kind {
				case sseError:
					return nil, &StreamError{KindValue: event.Class, Status: event.Status, Msg: event.Raw,
						RetryAfterSecs: event.RetryAfterSecs, ServiceUnavailable: event.ServiceUnavailable}
				case sseDone:
					if !acc.sawAny {
						return nil, ErrEmptyStream
					}
					return acc.result(), nil
				case sseChunk:
					acc.add(event.Chunk)
				}
			}
		}
		if err == io.EOF {
			for _, data := range buf.finish() {
				event := parseSSELine(data)
				switch event.Kind {
				case sseError:
					return nil, &StreamError{KindValue: event.Class, Status: event.Status, Msg: event.Raw,
						RetryAfterSecs: event.RetryAfterSecs, ServiceUnavailable: event.ServiceUnavailable}
				case sseDone:
					if !acc.sawAny {
						return nil, ErrEmptyStream
					}
					return acc.result(), nil
				case sseChunk:
					acc.add(event.Chunk)
				}
			}
			if !acc.sawAny {
				return nil, ErrEmptyStream
			}
			return acc.result(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// ---------------------------------------------------------------------------
// 流式

// Stream 转发 SSE（不关心错误回调）。
func Stream(w http.ResponseWriter, r io.Reader) error {
	_, err := StreamWithError(w, r, nil)
	return err
}

// StreamWithError 把上游信封里的内层 chunk 转成 OpenAI SSE 帧写给客户端。
// 遇到流内业务错误时回调 onErr（非 nil）并注入错误帧收尾；返回末帧 usage。
func StreamWithError(w http.ResponseWriter, r io.Reader, onErr func(*StreamError)) (map[string]any, error) {
	fl, _ := w.(http.Flusher)
	var wrote bool
	ensureHeaders := func() {
		if wrote {
			return
		}
		wrote = true
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
	}
	write := func(frame map[string]any) error {
		ensureHeaders()
		raw, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	writeRaw := func(s string) error {
		ensureHeaders()
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	acc := newAccumulator()
	guard := &reasoningGuard{}
	roleSent := false
	frame := func(d map[string]any, finish string) map[string]any {
		choice := map[string]any{"index": 0, "delta": d}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk := map[string]any{
			"id":      acc.id,
			"object":  "chat.completion.chunk",
			"created": acc.created,
			"model":   acc.model,
			"choices": []any{choice},
		}
		return chunk
	}
	// send 写一帧；首帧前补 role（OpenAI 客户端按它认领流）。
	send := func(d map[string]any, finish string) error {
		if !roleSent {
			roleSent = true
			if err := write(frame(map[string]any{"role": "assistant", "content": ""}, "")); err != nil {
				return err
			}
		}
		chunk := frame(d, finish)
		if acc.usage != nil && finish != "" {
			chunk["usage"] = acc.usage
		}
		return write(chunk)
	}

	handle := func(event sseEvent) (bool, error) {
		switch event.Kind {
		case sseError:
			se := &StreamError{KindValue: event.Class, Status: event.Status, Msg: event.Message,
				RetryAfterSecs: event.RetryAfterSecs, ServiceUnavailable: event.ServiceUnavailable}
			if onErr != nil {
				onErr(se)
			}
			if !wrote {
				// 一个字节都没发：让调用方决定换号重试。
				return true, se
			}
			_ = write(map[string]any{"error": map[string]any{"message": event.Message, "type": "proxy_error", "detail": event.Raw}})
			_ = writeRaw("data: [DONE]\n\n")
			return true, nil
		case sseDone:
			return true, nil
		case sseChunk:
			deltas := acc.add(event.Chunk)
			for _, d := range deltas {
				if d.kind != deltaReasoning {
					continue
				}
				hit := guard.Push(d.text)
				if hit == nil {
					continue
				}
				// 思考链机械复读：模型侧问题，不罚号（onErr 走 ErrReasoningLoop 的空分支），
				// 但要把这段流掐掉——否则客户端会一直等一段永远重复的思考。
				se := &StreamError{KindValue: ErrReasoningLoop, Status: 502,
					Msg: "思考链复读（" + hit.Kind + "：" + hit.Detail + "），已中断本次生成"}
				if onErr != nil {
					onErr(se)
				}
				_ = write(map[string]any{"error": map[string]any{
					"message": se.Msg, "type": "proxy_error", "code": "upstream_reasoning_loop"}})
				_ = writeRaw("data: [DONE]\n\n")
				return true, nil // 已收尾，不再继续读上游
			}
			for _, d := range deltas {
				payload := map[string]any{}
				switch d.kind {
				case deltaReasoning:
					payload["reasoning_content"] = d.text
				case deltaToolCall:
					call := map[string]any{"index": d.index, "type": "function", "function": map[string]any{}}
					if d.id != "" {
						call["id"] = d.id
					}
					if d.name != "" {
						call["function"].(map[string]any)["name"] = d.name
					}
					if d.arguments != "" {
						call["function"].(map[string]any)["arguments"] = d.arguments
					}
					payload["tool_calls"] = []any{call}
				default:
					payload["content"] = d.text
				}
				if err := send(payload, ""); err != nil {
					return true, err
				}
			}
		}
		return false, nil
	}

	buf := &lineBuffer{}
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			for _, data := range buf.push(raw) {
				stop, herr := handle(parseSSELine(data))
				if herr != nil {
					return nil, herr
				}
				if stop {
					return finishStream(acc, send, func() error { return writeRaw("data: [DONE]\n\n") })
				}
			}
		}
		if err == io.EOF {
			for _, data := range buf.finish() {
				stop, herr := handle(parseSSELine(data))
				if herr != nil {
					return nil, herr
				}
				if stop {
					return finishStream(acc, send, func() error { return writeRaw("data: [DONE]\n\n") })
				}
			}
			return finishStream(acc, send, func() error { return writeRaw("data: [DONE]\n\n") })
		}
		if err != nil {
			return nil, err
		}
	}
}

// finishStream 收尾：空流报错（调用方还能换号），否则补 finish_reason 与 [DONE]。
func finishStream(acc *accumulator, send func(map[string]any, string) error, done func() error) (map[string]any, error) {
	if !acc.sawAny && acc.finish == "" {
		return nil, ErrEmptyStream
	}
	finish := acc.finish
	if len(acc.tools) > 0 {
		finish = "tool_calls"
	}
	if finish == "" {
		finish = "stop"
	}
	if err := send(map[string]any{}, finish); err != nil {
		return nil, err
	}
	if err := done(); err != nil {
		return nil, err
	}
	return acc.usage, nil
}

// newResponseID 生成 chatcmpl-<24 hex>。
func newResponseID() string {
	uuid := randomUUIDOrEmpty()
	compact := strings.ReplaceAll(uuid, "-", "")
	if len(compact) > 24 {
		compact = compact[:24]
	}
	return "chatcmpl-" + compact
}

func randomUUID() string { return randomUUIDOrEmpty() }

// stripThinkingTags 去掉文本里的思考标签。
func stripThinkingTags(text string) string {
	out := text
	for _, pair := range thinkTags {
		out = strings.ReplaceAll(out, pair[0], "")
		out = strings.ReplaceAll(out, pair[1], "")
	}
	return out
}

// intOf 把 JSON 数字/数字字符串归一成 int。
func intOf(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return int(n), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return n, true
		}
	}
	return 0, false
}
