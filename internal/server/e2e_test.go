package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/reqlog"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// fakeUpstream 是假的小浣熊上游：断言出站形状，并按需返回流/非流/错误。
type fakeUpstream struct {
	srv       *httptest.Server
	chatHits  atomic.Int64
	lastBody  atomic.Value // []byte
	lastAuth  atomic.Value // string
	rejectTo  atomic.Value // string：非空时拒绝新 token，模拟过期
	refreshOK bool
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{refreshOK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/web/llm/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.chatHits.Add(1)
		f.lastBody.Store(body)
		f.lastAuth.Store(r.Header.Get("Authorization"))
		if want, _ := f.rejectTo.Load().(string); want != "" && r.Header.Get("Authorization") == "Bearer "+want {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":2001,"message":"token expired"}`))
			return
		}
		var req struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"`+req.Model+
				`","choices":[{"index":0,"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":11,"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":2}}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		frames := []string{
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"upstream-internal","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"想一下"}}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"upstream-internal","choices":[{"index":0,"delta":{"content":"你好"}}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"upstream-internal","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
		}
		for _, frame := range frames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	mux.HandleFunc("/api/web/llm/v2/model_catalog", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"categories":[{"type":"chat","models":[`+
			`{"name":"sn-glm-5-3-flash","display_name":"GLM 5.3 Flash","context_window":200000,"max_tokens":8000,"visible":true},`+
			`{"name":"raccoon-chat-ml-5-5","display_name":"Raccoon Chat","context_window":"180000","params":{"max_tokens":80000},"visible":false}]}]}}`)
	})
	mux.HandleFunc("/api/web/auth/v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.RefreshToken != "old-refresh" || !f.refreshOK {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":2001,"message":"refresh rejected"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"new-access","refresh_token":"new-refresh"}}`)
	})
	mux.HandleFunc("/api/web/points/v1/balance", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":1234,"daily_points":10,"monthly_points":100,"reward_points":24,"topup_points":1100}}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newTestHandler 用假上游装配一个 handler（一个账号）。
func newTestHandler(t *testing.T, f *fakeUpstream) (*Handler, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	client := upstream.New(upstream.Options{
		LLMBase: f.srv.URL + "/api/web/llm/v2", AuthBase: f.srv.URL + "/api/web/auth/v1",
		PointsBase: f.srv.URL + "/api/web/points/v1", Origin: f.srv.URL,
		Connect: 5 * time.Second, Header: 5 * time.Second,
	})
	acct := auth.New("access-1", "old-refresh", "一号")
	acct.ApplyTokens("access-1", "old-refresh")
	acct.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	if err := acct.SaveAtomic(dir); err != nil {
		t.Fatalf("保存账号: %v", err)
	}
	p := pool.New([]*auth.Auth{acct}, time.Minute)
	h := NewHandler(Config{
		Pool: p, Upstream: client, AuthDir: dir, APIKey: "sk-test",
		MaxRotate: 2, RefreshSkew: 5 * time.Minute, DefaultModel: "sn-glm-5-3-flash",
		Log: reqlog.New(50), Version: "test",
	})
	return h, p
}

func post(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChatPassThroughRewritesModel(t *testing.T) {
	f := newFakeUpstream(t)
	h, _ := newTestHandler(t, f)

	rec := post(t, h, "/v1/chat/completions",
		`{"model":"sn-glm-5-3-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if out.Model != "sn-glm-5-3-flash" {
		t.Fatalf("model 未回写为客户端名: %q", out.Model)
	}
	// 出站体必须是原样透传（除 model 外不改字段）
	var sent map[string]any
	_ = json.Unmarshal(f.lastBody.Load().([]byte), &sent)
	if sent["temperature"] != 0.3 {
		t.Fatalf("出站体丢了字段: %v", sent)
	}
	if got := f.lastAuth.Load().(string); got != "Bearer access-1" {
		t.Fatalf("出站 Authorization = %q", got)
	}
}

func TestChatStreamRewritesModelAndKeepsDone(t *testing.T) {
	f := newFakeUpstream(t)
	h, _ := newTestHandler(t, f)
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"sn-glm-5-3-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("缺 [DONE]: %s", body)
	}
	if strings.Contains(body, "upstream-internal") {
		t.Fatalf("上游内部模型名泄漏给客户端: %s", body)
	}
	if !strings.Contains(body, `"model":"sn-glm-5-3-flash"`) {
		t.Fatalf("流里未回写模型名: %s", body)
	}
}

// 401 强制刷新后用新 token 重试，仍返回 200（同号不再换号）。
func TestAuthFailureRefreshesAndRetries(t *testing.T) {
	f := newFakeUpstream(t)
	f.rejectTo.Store("access-1")
	h, p := newTestHandler(t, f)

	rec := post(t, h, "/v1/chat/completions", `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("刷新后仍失败, 状态码=%d body=%s", rec.Code, rec.Body.String())
	}
	acct := p.Accounts()[0]
	if acct.Token() != "new-access" || acct.RefreshValue() != "new-refresh" {
		t.Fatalf("新票未落地: %q / %q", acct.Token(), acct.RefreshValue())
	}
	if f.chatHits.Load() != 2 {
		t.Fatalf("期望 2 次上游调用（旧票 401 + 新票成功），实际 %d", f.chatHits.Load())
	}
}

func TestAnthropicMessagesNonStream(t *testing.T) {
	f := newFakeUpstream(t)
	h, _ := newTestHandler(t, f)
	rec := post(t, h, "/v1/messages", `{
		"model":"sn-glm-5-3-flash","max_tokens":256,
		"system":"你是助手","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if out["type"] != "message" {
		t.Fatalf("不是 Anthropic 消息形状: %s", rec.Body.String())
	}
	blocks, _ := out["content"].([]any)
	if len(blocks) == 0 {
		t.Fatalf("content 为空: %s", rec.Body.String())
	}
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "你好" {
		t.Fatalf("正文块不对: %v", first)
	}
	// 出站必须把 system 放回首条消息
	var sent struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(f.lastBody.Load().([]byte), &sent)
	if len(sent.Messages) == 0 || sent.Messages[0].Role != "system" {
		t.Fatalf("system 未落到 messages[0]: %+v", sent.Messages)
	}
}

func TestAnthropicMessagesStream(t *testing.T) {
	f := newFakeUpstream(t)
	h, _ := newTestHandler(t, f)
	rec := post(t, h, "/v1/messages", `{
		"model":"sn-glm-5-3-flash","max_tokens":256,"stream":true,
		"messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺 %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"text":"你好"`) {
		t.Fatalf("正文没出来:\n%s", body)
	}
}

func TestResponsesStream(t *testing.T) {
	f := newFakeUpstream(t)
	h, _ := newTestHandler(t, f)
	rec := post(t, h, "/v1/responses", `{
		"model":"sn-glm-5-3-flash","stream":true,
		"input":[{"role":"user","content":[{"type":"input_text","text":"你好"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺 %q:\n%s", want, body)
		}
	}
}

func TestUnknownModelRejected(t *testing.T) {
	f := newFakeUpstream(t)
	h, _ := newTestHandler(t, f)
	rec := post(t, h, "/v1/chat/completions", `{"model":"nope","messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知模型应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if f.chatHits.Load() != 0 {
		t.Fatalf("未知模型不该打上游")
	}
}

func TestModelsListUsesCatalog(t *testing.T) {
	f := newFakeUpstream(t)
	h, _ := newTestHandler(t, f)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Data) != 1 || out.Data[0].ID != "sn-glm-5-3-flash" {
		t.Fatalf("可见模型应为 1 条（visible:false 的过滤掉）: %+v", out.Data)
	}
}

func TestUnauthorized(t *testing.T) {
	f := newFakeUpstream(t)
	h, _ := newTestHandler(t, f)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺密钥应 401，实际 %d", rec.Code)
	}
}

// 两条协议并发打同一账号：不能出现数据竞争（-race 下跑这条）。
func TestConcurrentProtocols(t *testing.T) {
	f := newFakeUpstream(t)
	h, _ := newTestHandler(t, f)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}[i%3]
			post(t, h, path, `{"model":"sn-glm-5-3-flash","messages":[{"role":"user","content":"hi"}],"input":[{"role":"user","content":"hi"}]}`)
		}(i)
	}
	wg.Wait()
	if f.chatHits.Load() == 0 {
		t.Fatalf("一次上游调用都没有")
	}
	// 逐帧解析一遍，确认流没被交错破坏
	rec := post(t, h, "/v1/chat/completions", `{"model":"sn-glm-5-3-flash","messages":[],"stream":true}`)
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &probe); err != nil {
			t.Fatalf("坏帧: %q", line)
		}
	}
}
