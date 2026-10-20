package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 额度口径：Qoder 按 credits 计费；美元口径保留字段以兼容面板既有渲染（本仓恒 credit）。
const (
	UnitCredit = "credit"
	UnitUSD    = "usd"
)

// EntUsage 是额度快照。
type EntUsage struct {
	Unit              string
	Remain, Total     int64
	UsedUSD, LimitUSD float64
	Expire            int64 // Unix 秒；0 = 无到期信息
}

// ExpireAt 统一的到期时刻。
func (u EntUsage) ExpireAt() int64 { return u.Expire }

// RemainUSD 剩余美元（不足 0 钳 0）。
func (u EntUsage) RemainUSD() float64 {
	if u.LimitUSD <= 0 {
		return 0
	}
	remain := u.LimitUSD - u.UsedUSD
	if remain < 0 {
		return 0
	}
	return remain
}

// CreditPackage 是「积分构成」页的一行（字段名与面板前端一致）。
type CreditPackage struct {
	Group     string  `json:"group,omitempty"`
	Name      string  `json:"name"`
	Unit      string  `json:"unit"`
	Remain    int64   `json:"remain"`
	Size      int64   `json:"size"`
	Used      int64   `json:"used,omitempty"`
	Expire    int64   `json:"expire,omitempty"`
	Start     int64   `json:"start,omitempty"`
	RemainUSD float64 `json:"remain_usd,omitempty"`
	UsedUSD   float64 `json:"used_usd,omitempty"`
	LimitUSD  float64 `json:"limit_usd,omitempty"`
}

// FetchQuota 拉原始额度响应。
func (c *Client) FetchQuota(a *authState) (map[string]any, error) {
	resp, err := c.doJSON(http.MethodGet, c.apiBase(a.Realm())+UsagePath, nil, OpenAPIHeaders(a.JWT()))
	if err != nil {
		return nil, err
	}
	return payloadOf(resp, "额度查询")
}

// FetchPlan 拉套餐信息（best-effort，调用方吞错）。
func (c *Client) FetchPlan(a *authState) (map[string]any, error) {
	resp, err := c.doJSON(http.MethodGet, c.apiBase(a.Realm())+PlanPath, nil, OpenAPIHeaders(a.JWT()))
	if err != nil {
		return nil, err
	}
	return payloadOf(resp, "套餐查询")
}

// orgBucket 取组织资源包。总量字段叫 cap（不是 total），另有 available 布尔标它是否真发放：
// 实测形状（agent2api 记录，国际版 Teams 号）
// `{"used":0,"remaining":0,"percentage":0,"unit":"credits","cap":0,"available":false}`
// —— 空桶也会整个键返回，所以「键在不在」不是判据。
func orgBucket(raw map[string]any) (map[string]any, int64) {
	org, ok := raw["orgResourcePackage"].(map[string]any)
	if !ok {
		return nil, 0
	}
	if avail, has := org["available"].(bool); has && !avail {
		return nil, 0
	}
	cap := int64Of(org["cap"])
	if cap == 0 {
		cap = int64Of(org["total"]) // 老形状兜底
	}
	if cap <= 0 && int64Of(org["remaining"]) <= 0 {
		return nil, 0
	}
	return org, cap
}

// UserEntUsageDetail 拉额度并归一成面板口径：个人额度 + 资源包剩余 + 组织包剩余。
func (c *Client) UserEntUsageDetail(a *authState) (EntUsage, error) {
	raw, err := c.FetchQuota(a)
	if err != nil {
		return EntUsage{}, err
	}
	quota, _ := raw["userQuota"].(map[string]any)
	addOn, _ := raw["addOnQuota"].(map[string]any)
	remain := int64Of(quota["remaining"]) + int64Of(addOn["remaining"])
	total := int64Of(quota["total"]) + int64Of(addOn["total"])
	// 组织包只有 available:true 时才算（见 orgBucket）。
	if org, orgTotal := orgBucket(raw); org != nil {
		remain += int64Of(org["remaining"])
		total += orgTotal
	}
	out := EntUsage{Unit: UnitCredit, Remain: remain, Total: total}
	out.Expire = int64Of(raw["expiresAt"]) / 1000
	if out.Expire == 0 {
		out.Expire = int64Of(raw["expireTime"]) / 1000
	}
	if addOn == nil && quota == nil {
		// 空结构 = 上游返回了异常形状（未登录/风控/结构变了），**不是**「剩余 0」。
		// 当成 0 会把好号的额度静默清零（面板显示 0、选号权重一起塌）。
		return EntUsage{}, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder 额度响应缺少 userQuota 字段"}
	}
	return out, nil
}

// UserEntUsage 是老接口形状（剩余/总额/到期秒），面板与调度器仍会调它。
func (c *Client) UserEntUsage(a *authState) (remain, total, expire int64, err error) {
	u, err := c.UserEntUsageDetail(a)
	if err != nil {
		return 0, 0, 0, err
	}
	return u.Remain, u.Total, u.Expire, nil
}

// BalancePackages 拉额度并把「套餐 / 资源包 / 组织包」摊平成面板的积分构成。
func (c *Client) BalancePackages(a *authState) ([]CreditPackage, EntUsage, error) {
	raw, err := c.FetchQuota(a)
	if err != nil {
		return nil, EntUsage{}, err
	}
	u, err := c.UserEntUsageDetail(a)
	if err != nil {
		return nil, EntUsage{}, err
	}
	planName := c.planName(a)
	never := int64(0)
	if planName == "" {
		planName = "个人套餐"
	}
	packs := []CreditPackage{}
	if quota, ok := raw["userQuota"].(map[string]any); ok {
		packs = append(packs, CreditPackage{
			Group:  "个人额度",
			Name:   planName,
			Unit:   UnitCredit,
			Remain: int64Of(quota["remaining"]),
			Size:   int64Of(quota["total"]),
			Used:   int64Of(quota["used"]),
			Expire: u.Expire,
			Start:  never,
		})
	}
	if addOn, ok := raw["addOnQuota"].(map[string]any); ok && (int64Of(addOn["total"]) > 0 || int64Of(addOn["remaining"]) > 0) {
		packs = append(packs, CreditPackage{
			Group:  "资源包",
			Name:   "加量资源包",
			Unit:   UnitCredit,
			Remain: int64Of(addOn["remaining"]),
			Size:   int64Of(addOn["total"]),
			Used:   int64Of(addOn["used"]),
			Expire: int64Of(addOn["expiresAt"]) / 1000,
		})
	}
	if org, orgTotal := orgBucket(raw); org != nil {
		packs = append(packs, CreditPackage{
			Group:  "组织额度",
			Name:   "组织资源包",
			Unit:   UnitCredit,
			Remain: int64Of(org["remaining"]),
			Size:   orgTotal,
			Used:   int64Of(org["used"]),
		})
	}
	return packs, u, nil
}

// CreditPackages 是旧签名（剩余/总额）的兼容包装。
func (c *Client) CreditPackages(a *authState) ([]CreditPackage, int64, int64, error) {
	packs, u, err := c.BalancePackages(a)
	return packs, u.Remain, u.Total, err
}

// planName 取套餐名（失败按空处理：套餐名只是展示项）。
func (c *Client) planName(a *authState) string {
	data, err := c.FetchPlan(a)
	if err != nil {
		return ""
	}
	return text(data, "plan_tier_name", "planName", "name")
}

// ---------------------------------------------------------------------------
// 签到 / 活动领取

// 活动类型与状态（与上游 sash 接口一致）。
const (
	actionClaimBenefit = "CLAIM_BENEFIT"
	statusClaimable    = "CLAIMABLE"
	statusClaimed      = "CLAIMED"
	resultAlreadyClaim = "ALREADY_CLAIMED"
)

// CheckinStatus 查今日签到状态：checkedIn = 已有 CLAIM_BENEFIT 被领取；
// enable = 当前有签到活动（窗口开着）；credits = 当前剩余额度（面板展示用）。
func (c *Client) CheckinStatus(a *authState) (checkedIn bool, credits int64, enable bool, err error) {
	campaigns, err := c.fetchCampaigns(a)
	if err != nil {
		return false, 0, false, err
	}
	for _, raw := range asSlice(campaigns["campaigns"]) {
		campaign, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text(campaign, "actionType") != actionClaimBenefit {
			continue
		}
		enable = true
		if text(campaign, "claimStatus") == statusClaimed {
			checkedIn = true
		}
	}
	if u, uerr := c.UserEntUsageDetail(a); uerr == nil {
		credits = u.Remain
	}
	return checkedIn, credits, enable, nil
}

// CheckinClaim 领取今日签到奖励（幂等：已领返回 ErrCheckinAlready）。
func (c *Client) CheckinClaim(a *authState) error {
	campaigns, err := c.fetchCampaigns(a)
	if err != nil {
		return err
	}
	var claimable map[string]any
	claimed := false
	for _, raw := range asSlice(campaigns["campaigns"]) {
		campaign, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text(campaign, "actionType") != actionClaimBenefit {
			continue
		}
		switch text(campaign, "claimStatus") {
		case statusClaimable:
			if claimable == nil {
				claimable = campaign
			}
		case statusClaimed:
			claimed = true
		}
	}
	if claimable == nil {
		if claimed {
			return ErrCheckinAlready
		}
		return &BusinessError{Message: "当前没有可领取的签到活动"}
	}
	campaignID := text(claimable, "campaignId")
	if campaignID == "" {
		return &Error{Kind: ErrServer, Status: 502, Msg: "Qoder 签到活动缺少活动标识，无法领取"}
	}
	url := c.apiBase(a.Realm()) + CampaignsPath + "/" + campaignID + "/claim"
	headers := SashHeaders(a.JWT())
	headers["content-type"] = "application/json"
	headers["origin"] = c.apiBase(a.Realm())
	resp, err := c.doJSON(http.MethodPost, url, map[string]any{}, headers)
	if err != nil {
		return err
	}
	// 并发/重放：上游回 409 + {"result":"ALREADY_CLAIMED"}，按「今天已领」处理。
	if resp.Status == 409 || resultOf(resp.Body) == resultAlreadyClaim {
		return ErrCheckinAlready
	}
	body, err := payloadOf(resp, "签到领取")
	if err != nil {
		return err
	}
	if text(body, "result") == resultAlreadyClaim || truthy(body["replayed"]) {
		return ErrCheckinAlready
	}
	if text(body, "status") != statusClaimed {
		status := orDefault(text(body, "status"), "未知")
		return &BusinessError{Message: fmt.Sprintf("签到未完成（上游状态 %s）", status)}
	}
	return nil
}

// fetchCampaigns 拉活动列表。国际版需要设备风控头才能拿到签到活动，本仓没有移植
// UMID 组件：那里会返回空列表（面板/调度器按「今日无活动」处理，不反复重试）。
// shortcut: 国际版签到依赖 UMID 风控身份；需要时按 agent2api 的 providers/qoder/risk 补。
func (c *Client) fetchCampaigns(a *authState) (map[string]any, error) {
	url := c.apiBase(a.Realm()) + CampaignsPath + campaignsQuery
	resp, err := c.doJSON(http.MethodGet, url, nil, SashHeaders(a.JWT()))
	if err != nil {
		return nil, err
	}
	if resp.Status == 401 || resp.Status == 403 {
		return nil, &Error{Kind: ErrSessionDead, Status: 401, Msg: "登录态已失效，请重新登录"}
	}
	return payloadOf(resp, "签到活动查询")
}

// resultOf 从领取响应里取 result 字段（并发/重放标记）。
func resultOf(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return text(payload, "result")
}

// ---------------------------------------------------------------------------
// 账号导入

// ExchangePAT 用个人访问令牌换作业令牌，并补齐用户资料。
func (c *Client) ExchangePAT(ctx context.Context, pat, realm, machineID string) (*authState, error) {
	pat = strings.TrimSpace(pat)
	if pat == "" || strings.Contains(pat, "|") {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: "请填写有效的 Qoder 个人访问令牌（PAT）"}
	}
	realm = normalRealm(realm)
	resp, err := c.doJSON(http.MethodPost, c.apiBase(realm)+ExchangePath,
		map[string]any{"personal_token": pat}, OpenAPIHeaders(""))
	if err != nil {
		return nil, err
	}
	data, err := payloadOf(resp, "PAT 换取令牌")
	if err != nil {
		return nil, err
	}
	token := text(data, "token")
	jobRefresh := text(data, "refresh_token")
	if token == "" || strings.Contains(jobRefresh, "|") {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder PAT 响应缺少有效 token 或 refresh_token 格式无效"}
	}
	profile, err := c.profile(ctx, token, realm)
	if err != nil {
		return nil, err
	}
	uid := text(profile, "id", "userId", "user_id")
	if uid == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder 用户信息缺少用户标识"}
	}
	expires := millisOf(data["expires_at"])
	if expires == 0 {
		expires = time.Now().Add(24 * time.Hour).UnixMilli()
	}
	a := &authState{
		AccessToken:  token,
		RefreshToken: authPackPATRefresh(pat, jobRefresh, uid, machineID),
		ExpiresAt:    expires,
		RealmID:      realm,
		MachineID:    machineID,
		UID:          uid,
		Nickname:     truncate(text(profile, "name", "username"), 100),
		Email:        truncate(text(profile, "email"), 320),
	}
	if err := a.CompleteIdentity(machineID); err != nil {
		return nil, err
	}
	return a, nil
}

// PrepareAccount 解析面板粘贴的账号内容：PAT 走兑换，否则按凭证解析并补 userinfo。
func (c *Client) PrepareAccount(ctx context.Context, payload map[string]any, machineID string) (*authState, error) {
	realm := normalRealm(firstString(payload["mode"], payload["edition"]))
	pat := firstString(payload["pat"], payload["personalAccessToken"], payload["personal_token"])
	if pat != "" {
		return c.ExchangePAT(ctx, pat, realm, machineID)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	a, err := authParse(raw)
	if err != nil {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}
	if a.UID == "" {
		profile, perr := c.profile(ctx, a.JWT(), a.Realm())
		if perr != nil {
			return nil, perr
		}
		a.UID = text(profile, "id", "userId", "user_id")
		a.Nickname = truncate(text(profile, "name", "username"), 100)
		a.Email = truncate(text(profile, "email"), 320)
	}
	if err := a.CompleteIdentity(machineID); err != nil {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}
	return a, nil
}

// profile 拉 userinfo（best-effort 场景由调用方判空）。
func (c *Client) profile(ctx context.Context, token, realm string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase(realm)+UserInfoPath, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range OpenAPIHeaders(token) {
		req.Header.Set(key, value)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return payloadOf(resp, "用户信息查询")
}

// ---------------------------------------------------------------------------
// 设备授权（PKCE S256）

// devicePollPending Qoder 在「用户还没授权」时返回的两个状态。
var devicePollPending = map[int]bool{202: true, 404: true}

// DeviceLogin 是一次设备授权流程的状态。verifier 只留在后端，不进 authURL。
type DeviceLogin struct {
	client    *Client
	realm     string
	machineID string
	state     string
	authURL   string
	pollURL   string
}

// NewDeviceLogin 生成一次设备授权：随机 verifier/challenge/nonce，拼好授权页与轮询地址。
func NewDeviceLogin(client *Client, realm, machineID string) (*DeviceLogin, error) {
	if client == nil {
		return nil, &Error{Kind: ErrServer, Status: 500, Msg: "Qoder 客户端未初始化"}
	}
	realm = normalRealm(realm)
	verifier, err := randomSecret()
	if err != nil {
		return nil, err
	}
	state, err := randomSecret()
	if err != nil {
		return nil, err
	}
	nonce, err := randomUUIDValue()
	if err != nil {
		return nil, err
	}
	challenge := pkceChallenge(verifier)

	authURL := webOrigin(realm) + DeviceLoginPath + "?" + url.Values{
		"challenge":        {challenge},
		"challenge_method": {"S256"},
		"machine_id":       {machineID},
		"nonce":            {nonce},
	}.Encode()
	pollURL := client.apiBase(realm) + DevicePollPath + "?" + url.Values{
		"nonce":            {nonce},
		"verifier":         {verifier},
		"challenge_method": {"S256"},
	}.Encode()

	return &DeviceLogin{client: client, realm: realm, machineID: machineID, state: state, authURL: authURL, pollURL: pollURL}, nil
}

// State 是任务的对外标识（面板用它做 taskId）。
func (d *DeviceLogin) State() string { return d.state }

// AuthURL 是给用户在浏览器打开的授权页地址。
func (d *DeviceLogin) AuthURL() string { return d.authURL }

// Realm 返回本次授权的地区。
func (d *DeviceLogin) Realm() string { return d.realm }

// Poll 查一次授权结果。done=false 表示用户还没授权（继续按 2 秒轮询）。
func (d *DeviceLogin) Poll(ctx context.Context) (*authState, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.pollURL, nil)
	if err != nil {
		return nil, false, err
	}
	for key, value := range OpenAPIHeaders("") {
		req.Header.Set(key, value)
	}
	resp, err := d.client.do(req)
	if err != nil {
		return nil, false, err
	}
	if devicePollPending[resp.Status] {
		return nil, false, nil
	}
	data, err := payloadOf(resp, "设备授权")
	if err != nil {
		return nil, false, err
	}
	token := text(data, "token")
	if token == "" {
		return nil, false, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder 设备授权响应缺少 token"}
	}
	refreshToken := text(data, "refresh_token")
	if strings.Contains(refreshToken, "|") {
		return nil, false, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder 设备授权响应的 refresh_token 格式无效"}
	}
	uid := text(data, "user_id")
	expires := millisOf(data["expires_at"])
	if expires == 0 {
		expires = time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	}
	a := &authState{
		AccessToken:  token,
		RefreshToken: authPackRefresh(refreshToken, uid, d.machineID),
		ExpiresAt:    expires,
		RealmID:      d.realm,
		MachineID:    d.machineID,
		UID:          uid,
	}
	// userinfo 是补昵称/邮箱的 best-effort：拿不到且本来就没有 uid 才算失败。
	if profile, perr := d.client.profile(ctx, token, d.realm); perr == nil {
		if id := text(profile, "id", "userId", "user_id"); id != "" {
			if a.UID != "" && a.UID != id {
				return nil, false, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder 凭证中的用户与账号资料不一致"}
			}
			a.UID = id
		}
		a.Nickname = truncate(text(profile, "name", "username"), 100)
		a.Email = truncate(text(profile, "email"), 320)
	} else if a.UID == "" {
		return nil, false, perr
	}
	if err := a.CompleteIdentity(d.machineID); err != nil {
		return nil, false, err
	}
	return a, true, nil
}
