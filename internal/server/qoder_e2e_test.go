package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/misakano7545/qoder2api-panel/internal/auth"
	"github.com/misakano7545/qoder2api-panel/internal/pool"
	"github.com/misakano7545/qoder2api-panel/internal/upstream"
)

// fakeQoder 是最小上游：模型目录 + userinfo + 额度 + 推理 SSE（信封里装 OpenAI chunk）。
func fakeQoder(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	return fakeQoderQueue(t, chunks, 0, false)
}

// fakeQoderQueue 的前 queueN 次 chat 请求回「排队」（403 + 10605 + retryAfterSeconds），
// 之后正常出流——用来钉住「排队要等一会儿重发，而不是立刻 503」。
func fakeQoderQueue(t *testing.T, chunks []string, queueN int, envelopeQueue bool) *httptest.Server {
	t.Helper()
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/algo/api/v2/model/list"):
			_, _ = io.WriteString(w, `{"chat":[
				{"key":"qmodel_38max","display_name":"Qwen3.8-Max","is_reasoning":true,"is_vl":true,"enable":true,"max_input_tokens":200000,"price_factor":0.5,
				 "thinking_config":{"enabled":{"efforts":{"low":{},"medium":{},"xhigh":{}}}}},
				{"key":"dfmodel","display_name":"DeepSeek-Flash","is_reasoning":false,"is_vl":false,"enable":true}
			]}`)
			return
		case r.URL.Path == "/api/v1/userinfo":
			_, _ = io.WriteString(w, `{"id":"u-1","name":"测试号"}`)
			return
		case r.URL.Path == "/api/v2/quota/usage":
			_, _ = io.WriteString(w, `{"userQuota":{"remaining":120,"total":200,"used":80,"unit":"credits"},"expiresAt":1900000000000}`)
			return
		case r.URL.Path == "/api/v2/user/plan":
			_, _ = io.WriteString(w, `{"plan_tier_name":"Pro"}`)
			return
		case strings.HasSuffix(r.URL.Path, "/agent_chat_generation"):
			if int(seen.Add(1)) <= queueN {
				w.Header().Set("Content-Type", "application/json")
				if !envelopeQueue {
					w.WriteHeader(http.StatusForbidden) // 形态一：403 直接拒
					_, _ = io.WriteString(w, `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"auto\\\",\\\"queueType\\\":\\\"p3\\\",\\\"retryAfterSeconds\\\":1,\\\"serviceAvailable\\\":true}"}"}`)
					return
				}
				// 形态二：200 的信封里 statusCodeValue=403（实测上游就是这样回来的）
				inner := `{"code":"10605","message":"{\"isQueued\":true,\"retryAfterSeconds\":1,\"serviceAvailable\":true}"}`
				env, _ := json.Marshal(map[string]any{"statusCodeValue": 403, "body": inner})
				_, _ = io.WriteString(w, "data: "+string(env)+"\n\n")
				return
			}
			// 签名与编码体必须真的存在，否则「跑通」没有意义。
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
				t.Errorf("缺少 COSY 签名头")
			}
			if r.Header.Get("Cosy-Sigpath") != "/api/v2/service/pro/sse/agent_chat_generation" {
				t.Errorf("Cosy-Sigpath = %q", r.Header.Get("Cosy-Sigpath"))
			}
			if body, _ := io.ReadAll(r.Body); len(body) == 0 {
				t.Errorf("请求体为空（应先 EncodeBody 再签名）")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range chunks {
				envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": chunk})
				_, _ = io.WriteString(w, "data: "+string(envelope)+"\n\n")
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestHandler 组装一个只含一个国际账号的 handler。
func newTestHandler(t *testing.T, fakeURL string) *Handler {
	t.Helper()
	up := upstream.New()
	up.SetAgentHosts(fakeURL, fakeURL)
	up.SetAPIHosts(fakeURL, fakeURL)
	up.SetTimeouts(5*time.Second, 5*time.Second, 0)

	acct := &auth.Auth{
		AccessToken:  "dt-token",
		RefreshToken: auth.PackRefresh("drt-refresh", "u-1", "m-1"),
		ExpiresAt:    time.Now().Add(48 * time.Hour).UnixMilli(),
		RealmID:      auth.RealmGlobal,
		MachineID:    "m-1",
		UID:          "u-1",
		Nickname:     "测试号",
	}
	p := pool.New("")
	p.Add(acct)
	return NewHandler(Config{Pool: p, Upstream: up, DefaultModel: "Auto", MaxRotate: 1})
}

func TestChatCompletionsNonStream(t *testing.T) {
	fake := fakeQoder(t, []string{
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","model":"Qwen3.8-Max","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","model":"Qwen3.8-Max","choices":[{"index":0,"delta":{"content":"世界"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
	})
	h := newTestHandler(t, fake.URL)

	body := `{"model":"Qwen3.8-Max","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 {
		t.Fatalf("响应形态不对: %s", rec.Body.String())
	}
	if got := resp.Choices[0].Message.Content; got != "你好世界" {
		t.Errorf("content = %q, want 你好世界", got)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage["total_tokens"] == nil {
		t.Errorf("缺少 usage: %s", rec.Body.String())
	}
}

func TestChatCompletionsStream(t *testing.T) {
	fake := fakeQoder(t, []string{
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","model":"Qwen3.8-Max","choices":[{"index":0,"delta":{"reasoning_content":"想一想"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","model":"Qwen3.8-Max","choices":[{"index":0,"delta":{"content":"答案"},"finish_reason":"stop"}]}`,
	})
	h := newTestHandler(t, fake.URL)
	body := `{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("缺少首帧 role: %s", out)
	}
	if !strings.Contains(out, `"reasoning_content":"想一想"`) {
		t.Errorf("缺少思考增量: %s", out)
	}
	if !strings.Contains(out, `"content":"答案"`) {
		t.Errorf("缺少正文增量: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("缺少 [DONE]: %s", out)
	}
}

func TestChatCompletionsToolCalls(t *testing.T) {
	fake := fakeQoder(t, []string{
		`{"id":"c1","object":"chat.completion.chunk","model":"Qwen3.8-Max","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"Qwen3.8-Max","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"玉林\"}"}}]},"finish_reason":"tool_calls"}]}`,
	})
	h := newTestHandler(t, fake.URL)
	body := `{"model":"Qwen3.8-Max","messages":[{"role":"user","content":"天气"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls 没有合并: %s", rec.Body.String())
	}
	call := resp.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "get_weather" || call.Function.Arguments != `{"city":"玉林"}` {
		t.Errorf("tool call 合并错误: %+v", call)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

// 排队（10605）不是失败：等上游建议的 1 秒后重发同一请求，第二次直接出字。
func TestQueuedRequestIsRetried(t *testing.T) {
	fake := fakeQoderQueue(t, []string{
		`{"id":"c1","model":"Auto","choices":[{"index":0,"delta":{"content":"排队后成功"},"finish_reason":"stop"}]}`,
	}, 1, false)
	h := newTestHandler(t, fake.URL)
	body := `{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("排队后应重发成功，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "排队后成功") {
		t.Errorf("正文缺失: %s", rec.Body.String())
	}
	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Errorf("应等上游建议的 1 秒再重发，实际只花了 %v", waited)
	}
}

// 同一条排队语义，但上游用 200 信封承载 403（实测形态）：也必须等一会儿重发。
func TestEnvelopeQueuedRequestIsRetried(t *testing.T) {
	fake := fakeQoderQueue(t, []string{
		`{"id":"c1","model":"Auto","choices":[{"index":0,"delta":{"content":"信封排队后成功"},"finish_reason":"stop"}]}`,
	}, 1, true)
	h := newTestHandler(t, fake.URL)
	body := `{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "信封排队后成功") {
		t.Fatalf("信封排队应重发成功，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Errorf("应按 retryAfterSeconds 等 1 秒，实际 %v", waited)
	}
}

// 一直排队：重试预算（2 次）用尽后必须回 qoder_model_busy，而不是谎称「号都不可用」。
func TestQueueBudgetExhausted(t *testing.T) {
	fake := fakeQoderQueue(t, nil, 5, false) // 永远排队
	h := newTestHandler(t, fake.URL)
	body := `{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "qoder_model_busy") {
		t.Fatalf("应回 qoder_model_busy，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Errorf("应该先按上游建议重试 2 次，实际只花了 %v", elapsed)
	}
}

func TestUnknownModelIs400(t *testing.T) {
	fake := fakeQoder(t, nil)
	h := newTestHandler(t, fake.URL)
	body := `{"model":"does-not-exist","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}

func TestModelsList(t *testing.T) {
	fake := fakeQoder(t, nil)
	h := newTestHandler(t, fake.URL)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	foundCN, foundGlobal := false, false
	for _, m := range resp.Data {
		switch m.ID {
		case "cn:Qwen3.8-Max":
			foundCN = true
		case "global:Qwen3.8-Max":
			foundGlobal = true
		}
	}
	if !foundCN || !foundGlobal {
		t.Errorf("模型列表应把两个地区分开列出（cn=%v global=%v）: %s", foundCN, foundGlobal, rec.Body.String())
	}
}
