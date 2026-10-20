package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/reqlog"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// fakeRaccoon 是小浣熊上游的假实现：chat 吐标准 OpenAI SSE（含 reasoning 与工具调用），
// 目录/额度/续期各自回一份真形状的信封。
type fakeRaccoon struct {
	refreshCalls int
	lastAuth     string
	lastBody     map[string]any
}

func (f *fakeRaccoon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		f.lastBody = map[string]any{}
		_ = json.Unmarshal(body, &f.lastBody)
		var peek struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &peek)
		if !peek.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"upstream-internal",`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"upstream-internal","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"upstream-internal","choices":[{"index":0,"delta":{"reasoning_content":"想一想"}}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"upstream-internal","choices":[{"index":0,"delta":{"content":"你好"}}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"upstream-internal","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]}}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"upstream-internal","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`,
		}
		for _, frame := range frames {
			if _, err := io.WriteString(w, "data: "+frame+"\n\n"); err != nil {
				return
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("/model_catalog", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"categories":[{"type":"chat","models":[
			{"name":"raccoon-chat-ml-5-5","display_name":"Raccoon Chat","tags":["thinking"],
			 "context_window":"180000","max_tokens":80000,"billing_multiplier":1.5,"visible":true}]}]}}`)
	})
	mux.HandleFunc("/balance", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":120,"daily_points":30,
			"monthly_points":50,"reward_points":10,"topup_points":30}}`)
	})
	mux.HandleFunc("/api/web/office/v3/setting_info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
	})
	mux.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
		f.refreshCalls++
		_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"fresh-access","refresh_token":"fresh-refresh","user_id":"u-1"}}`)
	})
	mux.HandleFunc("/login_with_authorization_code", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"code-access","refresh_token":"code-refresh","office_org_name":"测试企业"}}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	return mux
}

// newTestHandler 建一个指向假上游的 handler（单账号、不鉴权）。
func newTestHandler(t *testing.T, upURL string) *Handler {
	t.Helper()
	client := upstream.New()
	client.SetAgentHosts(upURL, upURL)
	client.SetAPIHosts(upURL, upURL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "access-1", RefreshToken: "refresh-1", UID: "u-1", Nickname: "测试号",
		ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
	}})
	return NewHandler(Config{
		Pool: p, Upstream: client, APIKey: "", MaxRotate: 1,
		RefreshSkew: 5 * time.Minute, DefaultModel: upstream.DefaultConfigName,
		RequestLog: reqlog.New(reqlog.Config{}),
	})
}

func newFakeHandler(t *testing.T) (*Handler, *fakeRaccoon) {
	t.Helper()
	fake := &fakeRaccoon{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return newTestHandler(t, srv.URL), fake
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

// 非流式 chat：上游 model 名必须被改回客户端要的名字，usage 要透传。
func TestChatNonStream(t *testing.T) {
	h, fake := newFakeHandler(t)
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"raccoon-chat-ml-5-5","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "raccoon-chat-ml-5-5" {
		t.Errorf("model 没被改回客户端名: %v", out["model"])
	}
	if fake.lastBody["model"] != "raccoon-chat-ml-5-5" {
		t.Errorf("出站 model = %v", fake.lastBody["model"])
	}
	if fake.lastAuth != "Bearer access-1" {
		t.Errorf("出站鉴权头 = %q", fake.lastAuth)
	}
}

// 流式 chat：客户端应看到标准 OpenAI chunk（含 reasoning_content 与 tool_calls）。
func TestChatStream(t *testing.T) {
	h, _ := newFakeHandler(t)
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"raccoon-chat-ml-5-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"reasoning_content", "tool_calls", "[DONE]", "raccoon-chat-ml-5-5"} {
		if !strings.Contains(body, want) {
			t.Errorf("流里缺少 %q", want)
		}
	}
	if strings.Contains(body, "upstream-internal") {
		t.Errorf("上游内部模型名泄漏到客户端: %s", body)
	}
}

// Anthropic 入口：Claude Code 发 messages，网关翻成 chat 再把 SSE 翻回 Anthropic 事件。
func TestMessagesStream(t *testing.T) {
	h, _ := newFakeHandler(t)
	rec := post(t, h, "/v1/messages", `{"model":"raccoon-chat-ml-5-5","max_tokens":64,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"message_start", "content_block_delta", "message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("Anthropic 事件流缺少 %q: %s", want, body)
		}
	}
}

// Responses 入口：Codex 发 /v1/responses，网关要有 response.completed 收尾。
func TestResponsesStream(t *testing.T) {
	h, _ := newFakeHandler(t)
	rec := post(t, h, "/v1/responses", `{"model":"raccoon-chat-ml-5-5","stream":true,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("Responses 事件流缺少 %q: %s", want, body)
		}
	}
}

// 未知模型：目录里没有就直接 400，不打上游。
func TestUnknownModelRejected(t *testing.T) {
	h, fake := newFakeHandler(t)
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fake.lastBody != nil {
		t.Errorf("未知模型不该打上游")
	}
}

// 没有可用账号：503 + no_healthy_account（客户端能分辨「号池空」与「请求坏了」）。
func TestNoAccount(t *testing.T) {
	client := upstream.New()
	client.SetAgentHosts("http://127.0.0.1:1", "http://127.0.0.1:1")
	h := NewHandler(Config{Pool: pool.New(""), Upstream: client, MaxRotate: 1,
		DefaultModel: upstream.DefaultConfigName})
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"raccoon-chat-ml-5-5","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("错误码不对: %s", rec.Body.String())
	}
}
