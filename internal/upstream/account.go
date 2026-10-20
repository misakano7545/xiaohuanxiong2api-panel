// account.go 账号侧口径：额度/积分、每日签到、凭证准备（含授权码换票）。
//
// 小浣熊只有一套账号体系：GET {points_base}/balance 给积分（available_points
// 及各来源分类），每日发放由服务端 00:00 自动入账（bills 流水可查），
// GET {points_base}/bills 给逐笔发放/到期流水。
package upstream

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// 计量单位：小浣熊按积分计费，面板显示「积分」。
const (
	UnitCredit = "积分"
	UnitUSD    = "USD"
)

// EntUsage 是额度快照（面板/调度器共用）。
type EntUsage struct {
	Remain  int64 `json:"remain"`
	Total   int64 `json:"total"`
	Expire  int64 `json:"expire,omitempty"`
	Daily   int64 `json:"daily,omitempty"`
	Monthly int64 `json:"monthly,omitempty"`
	Reward  int64 `json:"reward,omitempty"`
	Topup   int64 `json:"topup,omitempty"`
	// 面板/池子要的三件美元口径字段：小浣熊只有积分，恒 0 / 空。
	Unit     string  `json:"unit,omitempty"`
	UsedUSD  float64 `json:"used_usd,omitempty"`
	LimitUSD float64 `json:"limit_usd,omitempty"`
}

// ExpireAt 返回到期时刻（毫秒）。
func (u EntUsage) ExpireAt() int64 { return u.Expire }

// RemainUSD 是面板的美元口径占位：小浣熊只有积分，恒 0。
func (u EntUsage) RemainUSD() float64 { return 0 }

// entUsagePack 是面板「额度明细」里的一条。
type entUsagePack struct {
	Name    string `json:"name"`
	Balance int64  `json:"balance"`
}

// CreditPackage 是面板额度页的打包形状。字段是前端 app.js 的契约
// （pkBySource/pkAccountSegments 读 remain/size/expire），别再退回只有 credits 的窄版——
// 那会让构成页把每个桶都判成「已用完」（remain undefined → 0）。
type CreditPackage struct {
	Group  string `json:"group,omitempty"`
	Name   string `json:"name"`
	Unit   string `json:"unit"`
	Remain int64  `json:"remain"`
	Size   int64  `json:"size"`
	// Expire 是批次到期（unix 秒）；0 = 上游没给到期信息，前端如实不画到期分布。
	Expire int64 `json:"expire,omitempty"`
}

// FetchQuota 返回原始额度节点（面板排障用）。
func (c *Client) FetchQuota(a *auth.Auth) (map[string]any, error) {
	resp, err := c.doJSON(http.MethodGet, c.pointsBase()+BalancePath, nil, c.authHeader(a))
	if err != nil {
		return nil, err
	}
	return payloadOf(resp, "额度查询")
}

// FetchPlan 返回订阅节点：小浣熊没有订阅接口，交给同一个额度响应兜住。
func (c *Client) FetchPlan(a *auth.Auth) (map[string]any, error) { return c.FetchQuota(a) }

// UserEntUsageDetail 查一次积分余额并归一成 EntUsage。
func (c *Client) UserEntUsageDetail(a *auth.Auth) (EntUsage, error) {
	data, err := c.FetchQuota(a)
	if err != nil {
		return EntUsage{}, err
	}
	usage := EntUsage{
		Unit:    UnitCredit,
		Remain:  mustInt(data["available_points"]),
		Daily:   mustInt(data["daily_points"]),
		Monthly: mustInt(data["monthly_points"]),
		Reward:  mustInt(data["reward_points"]),
		Topup:   mustInt(data["topup_points"]),
	}
	if usage.Remain < 0 {
		return EntUsage{}, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊额度响应里没有 available_points"}
	}
	usage.Total = usage.Remain
	return usage, nil
}

// UserEntUsage 是老接口形状（remain,total,expire）。
func (c *Client) UserEntUsage(a *auth.Auth) (remain, total, expire int64, err error) {
	u, err := c.UserEntUsageDetail(a)
	return u.Remain, u.Total, u.Expire, err
}

// BalancePackages 返回面板额度页需要的钱包明细。
//
// 上游没有「资源包」概念，构成页口径 = balance 的四个来源分类（每日/每月会员/
// 奖励/充值），每桶 remain=size（余额本身没有"面额"概念，两者一致就是诚实口径）。
// 「可用积分」不单独成包——它就是四桶之和，再进列表会把构成条撑爆、和桶重复计数。
// 每日桶的到期取自 bills 里**最近未过期**的 daily_grant 批次（上游现在批次是发放
// 后 7 天滚动过期，不是午夜清零）；流水取不到就不猜，expire 留空。
// 全 0 的桶不输出：否则前端把每个空桶归进「已用完（面额合计 0）」，纯噪音。
func (c *Client) BalancePackages(a *auth.Auth) ([]CreditPackage, EntUsage, error) {
	usage, err := c.UserEntUsageDetail(a)
	if err != nil {
		return nil, EntUsage{}, err
	}
	dailyExpire := c.dailyBatchExpiry(a)
	wallet := []struct {
		name   string
		value  int64
		expire int64
	}{
		{"每日积分", usage.Daily, dailyExpire},
		{"每月会员积分", usage.Monthly, 0},
		{"奖励积分", usage.Reward, 0},
		{"充值积分", usage.Topup, 0},
	}
	packs := make([]CreditPackage, 0, len(wallet))
	for _, w := range wallet {
		if w.value <= 0 {
			continue
		}
		packs = append(packs, CreditPackage{
			Name: w.name, Unit: UnitCredit, Remain: w.value, Size: w.value, Expire: w.expire,
		})
	}
	a.SetCredit(usage.Remain)
	return packs, usage, nil
}

// dailyBatchExpiry 查 bills，取每日发放批次里最早的**未过期**到期时刻（unix 秒）。
// 失败静默返回 0（构成页照常显示余额，只是到期分布缺这一列——不拿流水故障拖累额度页）。
func (c *Client) dailyBatchExpiry(a *auth.Auth) int64 {
	resp, err := c.doJSON(http.MethodGet,
		c.pointsBase()+PointsBillsPath+"?"+PointsBillsQuery, nil, c.authHeader(a))
	if err != nil {
		return 0
	}
	data, err := payloadOf(resp, "积分流水")
	if err != nil {
		return 0
	}
	now := time.Now()
	var earliest int64
	items, _ := data["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil || text(item, "biz_type") != "daily_grant" {
			continue
		}
		exp := parseRFC3339(text(item, "expire_at"))
		if exp <= 0 || time.Unix(exp, 0).Before(now) {
			continue
		}
		if earliest == 0 || exp < earliest {
			earliest = exp
		}
	}
	return earliest
}

// parseRFC3339 把上游时间戳（2006-01-02T15:04:05+08:00）转 unix 秒；解析不了返回 0。
func parseRFC3339(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// CreditPackages 是 BalancePackages 的老签名（credits, total, expire）。
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	packs, usage, err := c.BalancePackages(a)
	return packs, usage.Remain, usage.Expire, err
}

// 每日积分按服务端 00:00（东八区）自动发放、午夜自动清零：bills 流水里
// 有当天的 daily_grant 即为「今日已到账」。不存在可触发的签到开关，面板的
// 「签到」只是登录领取检查 + 以流水为准的状态确认。
var shanghaiTZ = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}()

// UserCreatedAt 取账号注册时间（东八区语义由调用方解释）。
//
// 只用来区分「上游今天还没发每日积分」和「签到了但没落账」：小浣熊的每日积分发放是
// 服务端 00:00 的定时任务，当天新注册的号要等次日首批，此前 bills 里根本没有
// daily_grant——那不是签到失败，是还没到发放时点。
func (c *Client) UserCreatedAt(a *auth.Auth) (time.Time, bool) {
	if a == nil || a.JWT() == "" {
		return time.Time{}, false
	}
	resp, err := c.doJSON(http.MethodGet, c.authBase()+UserInfoPath, nil,
		map[string]string{"Accept": "application/json", "Authorization": "Bearer " + a.JWT()})
	if err != nil {
		return time.Time{}, false
	}
	data, err := payloadOf(resp, "用户资料")
	if err != nil {
		return time.Time{}, false
	}
	raw := text(data, "created_at", "register_time", "createdAt")
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, perr := time.Parse(layout, raw); perr == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// CheckinStatus 查「今日每日发放是否已到账」：口径是 bills 流水里今天有没有
// daily_grant；流水查不到时退回余额的 daily_points>0。credits=可用积分，
// enable=账号可用（余额 0 且今日无发放说明额度耗尽，交由调度器判断）。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	usage, err := c.UserEntUsageDetail(a)
	if err != nil {
		return false, 0, false, err
	}
	credits = usage.Remain
	if granted, berr := c.dailyGrantToday(a); berr == nil {
		checkedIn = granted
	} else {
		checkedIn = usage.Daily > 0
	}
	if checkedIn {
		a.MarkCheckedIn()
	}
	return checkedIn, credits, true, nil
}

// dailyGrantToday 读 bills 流水：今天（东八区）是否已有 daily_grant 入账。
func (c *Client) dailyGrantToday(a *auth.Auth) (bool, error) {
	resp, err := c.doJSON(http.MethodGet,
		c.pointsBase()+PointsBillsPath+"?"+PointsBillsQuery, nil, c.authHeader(a))
	if err != nil {
		return false, err
	}
	data, err := payloadOf(resp, "积分流水")
	if err != nil {
		return false, err
	}
	today := time.Now().In(shanghaiTZ).Format("2006-01-02")
	items, _ := data["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil || text(item, "biz_type") != "daily_grant" {
			continue
		}
		grantAt := text(item, "grant_at", "created_at")
		if len(grantAt) >= 10 && grantAt[:10] == today {
			return true, nil
		}
	}
	return false, nil
}

// CheckinClaim 走登录领取接口（上游幂等：今日已发放则 granted=false）。
// 只有 granted=true 才记本地台账——发放本体是服务端 00:00 做的，这里只是兜住
// 登录弹窗式的手动领取路径。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	resp, err := c.doJSON(http.MethodPost, c.desktopBase()+PointsGrantPath, map[string]any{}, c.authHeader(a))
	if err != nil {
		return err
	}
	data, err := payloadOf(resp, "登录领取积分")
	if err != nil {
		return err
	}
	if granted, _ := data["granted"].(bool); granted {
		a.MarkCheckedIn()
	}
	return nil
}

// PrepareAccount 把面板粘贴的内容变成账号：授权回调链接换票，或直接是凭证 JSON。
//
// payload 来自面板的导入端点：
//
//	{"access_token":"…","refresh_token":"…"}
//	{"callback":"office-raccoon://auth/callback?code=…&state=…"}
func (c *Client) PrepareAccount(_ context.Context, payload map[string]any, _ string) (*auth.Auth, error) {
	if raw, ok := payload["callback"].(string); ok && strings.TrimSpace(raw) != "" {
		code, _, valid := ParseCallbackCode(raw)
		if !valid {
			return nil, &Error{Kind: ErrClient, Status: 400, Msg: "回调链接不合法，应形如 office-raccoon://auth/callback?code=…&state=…"}
		}
		return c.exchangeCode(code)
	}
	if code, ok := payload["code"].(string); ok && strings.TrimSpace(code) != "" {
		return c.exchangeCode(strings.TrimSpace(code))
	}
	access := text(payload, "access_token", "accessToken", "token")
	refresh := text(payload, "refresh_token", "refreshToken")
	if access == "" && refresh == "" {
		return nil, &Error{Kind: ErrClient, Status: 400,
			Msg: "无法识别的内容：请粘贴小浣熊账号 JSON（access_token / refresh_token），或 office-raccoon:// 授权回调链接"}
	}
	account := &auth.Auth{Data: auth.Data{
		AccessToken:  stripBearerToken(access),
		RefreshToken: stripBearerToken(refresh),
		UID:          text(payload, "uid", "user_id", "userId"),
		Nickname:     text(payload, "nickname", "name"),
		ExpiresAt:    millisOf(payload["expires_at"]),
	}}
	if account.AccessToken == "" {
		// 只给了 refresh_token：先换一次票，顺带验证它还能用。
		probe := &auth.Auth{Data: auth.Data{UID: account.UID, RefreshToken: account.RefreshToken}}
		if err := c.RefreshToken(probe); err != nil {
			return nil, err
		}
		account.AccessToken, account.ExpiresAt = probe.JWT(), probe.ExpiresAtValue()
	}
	if account.ExpiresAt == 0 {
		account.ExpiresAt = auth.JWTExpiry(account.AccessToken)
	}
	if uid, nick, _, err := c.GetUserInfo(account); err == nil {
		if uid != "" {
			account.UID = uid
		}
		if account.Nickname == "" {
			account.Nickname = nick
		}
	}
	if account.Nickname == "" {
		account.Nickname = account.UID
	}
	if err := account.CompleteIdentity(""); err != nil {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}
	return account, nil
}

// exchangeCode 用一次性授权码换凭证。
func (c *Client) exchangeCode(code string) (*auth.Auth, error) {
	resp, err := c.doJSON(http.MethodPost, c.authBase()+LoginCodePath,
		map[string]string{"authorization_code": code}, c.clientIDHeaders())
	if err != nil {
		return nil, err
	}
	if code, ok := businessCodeOf(resp.Body); ok && code == CodeExpired {
		return nil, ErrCodeExpired
	}
	data, err := payloadOf(resp, "授权码换票")
	if err != nil {
		return nil, err
	}
	access := text(data, "access_token", "accessToken")
	refresh := text(data, "refresh_token", "refreshToken")
	if access == "" || refresh == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊未返回完整凭证（access_token/refresh_token）"}
	}
	account := &auth.Auth{Data: auth.Data{
		AccessToken:  stripBearerToken(access),
		RefreshToken: stripBearerToken(refresh),
		Nickname:     text(data, "office_org_name", "office_identity"),
		ExpiresAt:    auth.JWTExpiry(access),
	}}
	// 授权码换票的响应里带 user_id，但那是**平台 id**（pu-…，iam 域的），不是 office 域的
	// 用户 id——同一个人在 token 导入路径里存的是 4465567/7719383 这种 office id，两条路
	// 用不同 id 会让同一个号在池子里出现两行。所以这里以 user_info 的 id 为准，
	// 换票响应里的 user_id 只当兜底。
	account.UID = orDefault(text(data, "user_id", "uid", "userId"), auth.JWTUserID(access))
	if uid, nick, _, err := c.GetUserInfo(account); err == nil {
		if uid != "" {
			account.UID = uid
		}
		if nick != "" {
			account.Nickname = nick
		}
	}
	if account.UID == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊授权码换票未返回用户标识"}
	}
	if account.Nickname == "" {
		account.Nickname = account.UID
	}
	return account, nil
}

// DeviceLogin 是浏览器授权会话（不需要官方客户端，也不需要人工粘贴）。
//
// 官方网页授权页（抓包 [51] 起）本来就支持给「非官方桌面客户端」投递授权码：
//
//	/code/authorize?login_source=desktop&appname=…&state=<64 hex>
//	                &redirect=http://127.0.0.1:<port>/auth/callback
//	                &desktop_callback=loopback
//
// 页面（抓包 [68] 的 authorize chunk）校验：redirect 必须匹配
// ^http://127\.0\.0\.1:[1-9][0-9]{3,4}/auth/callback$、state 必须 64 位小写 hex、
// 且必须带 desktop_callback=loopback，否则退回默认的 office-raccoon:// 跳转。
// 同一处还认 vscode://sensetime.raccoon——这条通道是给 IDE/第三方客户端用的公开扩展点，
// 我们复用它，而不是去逆向客户端二进制。
//
// 于是流程是：面板给出授权页 URL → 用户在自己浏览器登录（图形验证码由页面自己做）
// → 页面把 code 打到我们自己的 /auth/callback → 面板 exchangeCode 换票入池。
type DeviceLogin struct {
	client  *Client
	realm   string
	machine string
	state   string
	url     string

	mu   sync.Mutex
	code string // 回调已经送到的授权码（单次使用）
}

// ErrLoopbackUnsupported 说明当前监听端口不能用于环回回调。
var ErrLoopbackUnsupported = errors.New(
	"环回回调要求面板监听在 127.0.0.1 的 1024-65535 端口；请改用「导入账号」粘贴授权回调链接或账号 JSON")

// NewDeviceLogin 建一次浏览器授权会话，callbackBase 形如 http://127.0.0.1:17867。
func NewDeviceLogin(client *Client, realm, machine, callbackBase string) (*DeviceLogin, error) {
	if client == nil {
		return nil, ErrLoopbackUnsupported
	}
	state, err := randState()
	if err != nil {
		return nil, err
	}
	callback := strings.TrimRight(callbackBase, "/") + "/auth/callback"
	// 与网页端同一条正则：只认 127.0.0.1 + 1024~65535 端口。写在前端能挡住的就不让上游拒。
	if !loopbackCallbackPattern.MatchString(callback) {
		return nil, ErrLoopbackUnsupported
	}
	query := url.Values{}
	query.Set("login_source", "desktop")
	query.Set("appname", "办公小浣熊客户端")
	query.Set("state", state)
	query.Set("redirect", callback)
	query.Set("desktop_callback", "loopback")
	return &DeviceLogin{
		client:  client,
		realm:   realm,
		machine: machine,
		state:   state,
		url:     client.originBase() + "/code/authorize?" + query.Encode(),
	}, nil
}

// loopbackCallbackPattern 抄自官方 authorize 页：端口必须 1024-65535（四位以上且不以 0 开头）。
var loopbackCallbackPattern = regexp.MustCompile(`^http://127\.0\.0\.1:[1-9][0-9]{3,4}/auth/callback$`)

// randState 生成 32 字节随机数的 hex（64 位小写），与网页端 state 校验同形。
func randState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// State 既是面板轮询用的会话 id，也是回调要核对的 state。
func (d *DeviceLogin) State() string { return d.state }

// Realm 返回地区。
func (d *DeviceLogin) Realm() string { return d.realm }

// AuthURL 返回授权页地址，面板展示 + 一键打开。
func (d *DeviceLogin) AuthURL() string { return d.url }

// AcceptCallback 收下浏览器送回的授权码（state 必须与本会话一致，且只收一次）。
func (d *DeviceLogin) AcceptCallback(state, code string) error {
	if subtle.ConstantTimeCompare([]byte(state), []byte(d.state)) != 1 {
		return errors.New("state 不匹配：这条回调不属于当前登录会话")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("回调里没有 code")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.code != "" {
		return errors.New("这个登录会话已经用过了，请回面板重新添加")
	}
	d.code = code
	return nil
}

// Poll 还没收到回调 → (nil,false,nil)；收到了 → 换票并返回凭证（授权码一次性，取走即清）。
func (d *DeviceLogin) Poll(context.Context) (*auth.Auth, bool, error) {
	d.mu.Lock()
	code := d.code
	d.code = ""
	d.mu.Unlock()
	if code == "" {
		return nil, false, nil
	}
	creds, err := d.client.exchangeCode(code)
	if err != nil {
		return nil, false, err
	}
	if creds != nil && d.machine != "" {
		if err := creds.CompleteIdentity(d.machine); err != nil {
			return nil, false, err
		}
	}
	return creds, true, nil
}

// ParseCallbackCode 从 office-raccoon://auth/callback?code=..&state=.. 取 code/state。
// 只认该 scheme+host+path，避免别的链接被当成回调。
func ParseCallbackCode(raw string) (code, state string, ok bool) {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexAny(raw, "\r\n"); i >= 0 {
		raw = raw[:i]
	}
	// 裸授权码（ac_…）直接认：用户在浏览器里可能只截到 code。
	if strings.HasPrefix(raw, "ac_") && !strings.Contains(raw, "://") && !strings.Contains(raw, "?") {
		return raw, "", true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	query := parsed.Query()
	switch {
	case strings.EqualFold(parsed.Scheme, "office-raccoon"):
		// 官方客户端回调：office-raccoon://auth/callback?code=…&state=…
		if parsed.Host != "auth" || parsed.Path != "/callback" {
			return "", "", false
		}
	case parsed.Scheme == "http" || parsed.Scheme == "https":
		// 我们的环回回调：http://127.0.0.1:<port>/auth/callback?code=…&state=…
		// 面板部署在服务器/局域网时，浏览器打不开这个本机地址，但地址栏里的链接是完整可用的——
		// 用户把它整条粘过来即可（host 不再限定 127.0.0.1：反正是我们自己的路径形状）。
		if parsed.Path != "/auth/callback" {
			return "", "", false
		}
	default:
		return "", "", false
	}
	code = query.Get("code")
	if code == "" {
		// 有的形态把授权码放 authorization_code（网页版口径）。
		code = query.Get("authorization_code")
	}
	return code, query.Get("state"), code != ""
}

// AuthorizeURL 构造网页授权页（面板展示给人手动打开用）。
func (c *Client) AuthorizeURL(appName string) string {
	if appName == "" {
		appName = "办公小浣熊客户端"
	}
	query := url.Values{}
	query.Set("login_source", "desktop")
	query.Set("client", "desktop")
	query.Set("app_name", appName)
	return c.originBase() + "/code/authorize?" + query.Encode()
}

func (c *Client) authHeader(a *auth.Auth) map[string]string {
	h := c.clientIDHeaders()
	if a == nil {
		h["Accept"] = "application/json"
		return h
	}
	h["Accept"] = "application/json"
	h["Authorization"] = "Bearer " + a.JWT()
	return h
}

// clientIDHeaders 复刻 v1.1.0 桌面客户端的识别头（抓包实测）：上游网关按
// 客户端渠道做风控，缺了这些头虽能过，但带上最贴近真实流量。
// x-client-version 可被 config.upstream.client_version 覆盖（官方升版本号时用）。
func (c *Client) clientIDHeaders() map[string]string {
	version := c.currentVersion()
	return map[string]string{
		"x-client-name":     "@raccoon/office-desktop",
		"x-client-platform": "desktop-windows-x64",
		"x-client-version":  version,
		"x-client-channel":  "official",
	}
}

func stripBearerToken(v string) string {
	v = strings.TrimSpace(v)
	return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
}

// mustInt 读整数字段；缺失/非法返回 -1（与「真的是 0」区分开）。
func mustInt(value any) int64 {
	if value == nil {
		return -1
	}
	if n, ok := asInt64(value); ok {
		return n
	}
	return -1
}

// businessCodeOf 从响应体里取业务码（没有返回 false）。
func businessCodeOf(body []byte) (int, bool) {
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, false
	}
	return intOf(envelope["code"])
}
