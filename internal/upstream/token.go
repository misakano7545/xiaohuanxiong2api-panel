package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Refresh 用 refresh_token 换新票。refresh 为空表示上游未轮换，调用方沿用旧值。
func (c *Client) Refresh(ctx context.Context, refreshToken string) (access, refresh string, err error) {
	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, data, err := c.doJSON(ctx, http.MethodPost, c.opts.AuthBase+"/refresh", body, "",
		map[string]string{"Content-Type": "application/json"}, maxErrorBytes)
	if err != nil {
		return "", "", err
	}
	var env apiEnvelope
	_ = json.Unmarshal(data, &env)
	if status < 200 || status >= 300 {
		return "", "", newError(status, data)
	}
	if env.Code != nil && *env.Code != 0 {
		return "", "", &Error{Kind: KindAuth, Status: status, Code: *env.Code, Message: env.text()}
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &payload)
	}
	if payload.AccessToken == "" {
		return "", "", &Error{Kind: KindAuth, Status: status, Message: "刷新未返回 access_token"}
	}
	return payload.AccessToken, payload.RefreshToken, nil
}

// LoginResult 是网页登录换回的凭证。
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	Identity     string
	OrgName      string
	OrgRole      string
}

// CodeExpired 是"授权码已失效"的上游业务码：不必重试，提示重新发起。
const CodeExpired = 200035

// ExchangeLoginCode 用一次性授权码换凭证。
func (c *Client) ExchangeLoginCode(ctx context.Context, code string) (*LoginResult, error) {
	body, err := json.Marshal(map[string]string{"authorization_code": code})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, data, err := c.doJSON(ctx, http.MethodPost, c.opts.AuthBase+"/login_with_authorization_code", body, "",
		map[string]string{"Content-Type": "application/json"}, maxErrorBytes)
	if err != nil {
		return nil, err
	}
	var env apiEnvelope
	_ = json.Unmarshal(data, &env)
	if env.Code != nil && *env.Code == CodeExpired {
		return nil, &Error{Kind: KindClient, Status: status, Code: *env.Code, Message: "授权码已失效，请重新发起登录"}
	}
	if status < 200 || status >= 300 {
		return nil, newError(status, data)
	}
	var payload map[string]json.RawMessage
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &payload)
	}
	text := func(key string) string {
		var s string
		if raw, ok := payload[key]; ok && json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	out := &LoginResult{
		AccessToken:  text("access_token"),
		RefreshToken: text("refresh_token"),
		Identity:     text("office_identity"),
		OrgName:      text("office_org_name"),
		OrgRole:      text("office_org_role"),
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return nil, &Error{Kind: KindClient, Status: status, Message: "上游未返回完整凭证（access_token/refresh_token）"}
	}
	return out, nil
}

// AuthorizeURL 构造网页授权页地址。state 由调用方生成并校验。
func (c *Client) AuthorizeURL(state, appName string) string {
	if appName == "" {
		appName = "办公小浣熊客户端"
	}
	q := url.Values{}
	q.Set("login_source", "desktop")
	q.Set("client", "desktop")
	q.Set("app_name", appName)
	q.Set("state", state)
	return c.opts.Origin + "/code/authorize?" + q.Encode()
}

// ParseCallbackCode 从 office-raccoon://auth/callback?code=..&state=.. 里取 code/state。
// 只认该 scheme+host+path，避免别的 scheme 被当成回调。
func ParseCallbackCode(raw string) (code, state string, ok bool) {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexAny(raw, "\r\n"); i >= 0 {
		raw = raw[:i] // 桌面端复制常带换行
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "office-raccoon") || u.Host != "auth" || u.Path != "/callback" {
		return "", "", false
	}
	q := u.Query()
	code = q.Get("code")
	return code, q.Get("state"), code != ""
}
