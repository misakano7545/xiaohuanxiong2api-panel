package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Balance 是积分余额。
type Balance struct {
	Available int64 `json:"available_points"`
	Daily     int64 `json:"daily_points"`
	Monthly   int64 `json:"monthly_points"`
	Reward    int64 `json:"reward_points"`
	Topup     int64 `json:"topup_points"`
}

// ErrNoBalance 表示上游响应里没带可用积分数。
var ErrNoBalance = &Error{Kind: KindUnknown, Message: "上游未返回可用积分"}

// Balance 查询积分余额（GET {points_base}/balance）。
func (c *Client) Balance(ctx context.Context, token string) (*Balance, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	status, data, err := c.doJSON(ctx, http.MethodGet, c.opts.PointsBase+"/balance", nil, token,
		map[string]string{"Accept": "application/json"}, maxJSONBytes)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, newError(status, data)
	}
	var env apiEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.Code != nil && *env.Code != 0 {
		return nil, &Error{Status: status, Code: *env.Code, Message: env.text()}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, ErrNoBalance
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, err
	}
	b := &Balance{
		Available: intOf(raw["available_points"]),
		Daily:     intOf(raw["daily_points"]),
		Monthly:   intOf(raw["monthly_points"]),
		Reward:    intOf(raw["reward_points"]),
		Topup:     intOf(raw["topup_points"]),
	}
	if _, ok := raw["available_points"]; !ok {
		return nil, ErrNoBalance
	}
	return b, nil
}

// ClaimDailyGrant 触发每日积分发放（GET {origin}/api/web/office/v3/setting_info）。
// 上游用这个接口把当天积分发到账，重复调用是安全的。
func (c *Client) ClaimDailyGrant(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	status, data, err := c.doJSON(ctx, http.MethodGet, c.opts.Origin+"/api/web/office/v3/setting_info", nil, token,
		map[string]string{
			"Accept":             "application/json",
			"X-Client-Platform":  "desktop-windows",
			"X-Client-Version":   "v1.0.0",
			"X-Raccoon-Language": "zh",
		}, maxJSONBytes)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return newError(status, data)
	}
	var env apiEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	if env.Code != nil && *env.Code != 0 {
		return &Error{Status: status, Code: *env.Code, Message: env.text()}
	}
	return nil
}

// intOf 把数字或数字字符串转成 int64；缺失/非法返回 -1。
func intOf(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return -1
	}
	var num json.Number
	if json.Unmarshal(raw, &num) == nil {
		if v, err := num.Int64(); err == nil {
			return v
		}
		var f float64
		if json.Unmarshal(raw, &f) == nil {
			return int64(f)
		}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		var n int64
		for _, r := range text {
			if r < '0' || r > '9' {
				return -1
			}
			n = n*10 + int64(r-'0')
		}
		return n
	}
	return -1
}
