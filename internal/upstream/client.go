// Package upstream 封装小浣熊（商汤 Raccoon）的上游接口。
//
// 聊天面就是 OpenAI 形状的 POST {llm_base}/chat/completions：请求体原样转发，
// SSE 原样回收，网关只做选号、改写 model 名与错误分类。其余接口（刷新 token、
// 模型目录、积分、每日发放、网页登录换码）各自一个文件。
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// 响应体读取上限。
const (
	maxJSONBytes  = 4 << 20
	maxErrorBytes = 64 << 10
)

// Kind 是错误分类，决定调用方换号、冷却还是原样透传。
type Kind int

const (
	KindUnknown Kind = iota
	KindAuth         // 401/403：token 失效，值得刷新后重试一次
	KindRate         // 429：账号级限流，冷却
	KindServer       // 5xx / 网络：可换号重试
	KindClient       // 其它 4xx：请求本身的问题，换号无用，原样透传
)

func (k Kind) String() string {
	switch k {
	case KindAuth:
		return "auth"
	case KindRate:
		return "rate"
	case KindServer:
		return "server"
	case KindClient:
		return "client"
	}
	return "unknown"
}

// Error 是带上游状态码与业务码的错误。
type Error struct {
	Kind    Kind
	Status  int
	Code    int
	Message string
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// Options 是构造客户端所需参数。
type Options struct {
	LLMBase    string
	AuthBase   string
	PointsBase string
	Origin     string
	UserAgent  string
	Connect    time.Duration
	Header     time.Duration
	Total      time.Duration
}

// Client 是共享的上游客户端。
type Client struct {
	http *http.Client
	opts Options
}

// New 构造客户端。不设 http.Client.Timeout（会截断长流式回答），总超时按请求套 context。
func New(opts Options) *Client {
	if opts.Connect <= 0 {
		opts.Connect = 30 * time.Second
	}
	if opts.Header <= 0 {
		opts.Header = 120 * time.Second
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "xhx2api-panel/0.1.0"
	}
	dialer := &net.Dialer{Timeout: opts.Connect}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: opts.Header,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &Client{http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, opts: opts}
}

// Options 返回生效参数（测试用来核对基址）。
func (c *Client) Options() Options { return c.opts }

// withTotal 套上请求级总超时（0 = 不套）。
func (c *Client) withTotal(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.opts.Total > 0 {
		return context.WithTimeout(ctx, c.opts.Total)
	}
	return context.WithCancel(ctx)
}

func (c *Client) newRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if c.opts.UserAgent != "" {
		req.Header.Set("User-Agent", c.opts.UserAgent)
	}
	return req, nil
}

// Chat 发一次聊天请求，成功（2xx）时返回未读的流；否则读完错误体并返回 *Error。
func (c *Client) Chat(ctx context.Context, token string, body []byte) (io.ReadCloser, int, error) {
	ctx, cancel := c.withTotal(ctx)
	req, err := c.newRequest(ctx, http.MethodPost, c.opts.LLMBase+"/chat/completions", body)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// cancel 交给调用方在关流时结束：把 cancel 包进 Close，避免整段流被提前掐断。
		return &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}, resp.StatusCode, nil
	}
	defer cancel()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	resp.Body.Close()
	return nil, resp.StatusCode, newError(resp.StatusCode, data)
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// doJSON 发一次请求并读完响应体。
func (c *Client) doJSON(ctx context.Context, method, url string, body []byte, token string, headers map[string]string, limit int64) (int, []byte, error) {
	req, err := c.newRequest(ctx, method, url, body)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return resp.StatusCode, data, err
}

// newError 把上游错误体转成 *Error。
func newError(status int, body []byte) *Error {
	e := &Error{Kind: Classify(status, body), Status: status, Message: extractMessage(body)}
	if e.Message == "" {
		text := strings.TrimSpace(string(body))
		if len(text) > 200 {
			text = text[:200]
		}
		if text == "" {
			text = "上游返回 " + fmt.Sprint(status)
		}
		e.Message = text
	}
	var probe struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Code != nil {
		e.Code = *probe.Code
	}
	return e
}

// Classify 按状态码与错误体给出分类。
func Classify(status int, body []byte) Kind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return KindAuth
	case status == http.StatusTooManyRequests:
		return KindRate
	case status >= 500:
		return KindServer
	case status >= 400:
		// 上游偶尔用 400 + 业务码表达限流/额度，识别出来才能正确冷却。
		var probe struct {
			Code *int `json:"code"`
		}
		if json.Unmarshal(body, &probe) == nil && probe.Code != nil {
			switch *probe.Code {
			case 1005, 1006, 2001, 2002, 4001, 4011:
				return KindRate
			}
		}
		return KindClient
	}
	return KindUnknown
}

// extractMessage 从错误体里取 message / msg / error.message。
func extractMessage(body []byte) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	for _, key := range []string{"message", "msg", "detail"} {
		var text string
		if raw, ok := payload[key]; ok && json.Unmarshal(raw, &text) == nil && text != "" {
			return text
		}
	}
	if raw, ok := payload["error"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) == nil {
			var text string
			if json.Unmarshal(inner["message"], &text) == nil && text != "" {
				return text
			}
		}
	}
	return ""
}

// apiEnvelope 是上游统一的 {code,message,data} 信封。
type apiEnvelope struct {
	Code    *int            `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

func (e apiEnvelope) text() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Msg
}
