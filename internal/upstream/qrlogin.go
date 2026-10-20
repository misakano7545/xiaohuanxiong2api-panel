// qrlogin.go 微信扫码登录：不需要官方客户端，也不需要浏览器回跳 127.0.0.1。
//
// 上游（抓包 + 实测）的扫码登录是一条**轮询式**通道：
//
//	POST {auth_base}/login_with_qrcode_code  {"qrcode_code": "<32 位 hex>"}
//	→ {"code":0,"data":{"status":"pending"}}           还没人扫
//	→ {"code":0,"data":{"status":"logging", "expired_at":…}}  已扫码、待确认
//	→ {"code":0,"data":{"status":"success", "access_token":…, "refresh_token":…}}
//	→ {"code":0,"data":{"status":"canceled"}}           用户取消/二维码失效
//
// 关键点：`qrcode_code` 由**客户端自己生成**（32 位小写 hex），服务端只认它并把
// 扫码结果挂在这个 code 上。所以服务端不需要任何回调地址——这正是它比
// office-raccoon:// 那条自定义协议链路更适合「面板部署在服务器/局域网」的原因：
// 二维码里编的是 `{origin}/login/mp?code=<code>&appname=…` 这个**公开网页**，
// 用户用微信扫完，我们这边轮询就能取票。
package upstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// QrCodePath 扫码登录的轮询端点（挂在 auth 域下）。
const QrCodePath = "/login_with_qrcode_code"

// qrAppName 二维码承载页展示的应用名（与官方客户端一致）。
const qrAppName = "商汤小浣熊官网"

// QrLogin 一次扫码登录会话。
type QrLogin struct {
	client *Client
	code   string // 32 位小写 hex，服务端据此关联扫码结果
	mach   string // 入池时补的设备指纹

	mu         sync.Mutex
	lastStatus string // 最近一次轮询到的上游状态（pending/logging/…），供面板显示进度
}

// LastStatus 最近一次轮询到的上游状态（中文文案；未轮询过返回「等待扫码」）。
func (q *QrLogin) LastStatus() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return QrStatusName(q.lastStatus)
}

// setStatus 记下本轮状态（并发安全：前端可能重叠轮询）。
func (q *QrLogin) setStatus(raw string) {
	q.mu.Lock()
	q.lastStatus = raw
	q.mu.Unlock()
}

// NewQrLogin 生成一个扫码会话：code 本地随机（16 字节 → 32 位 hex）。
func NewQrLogin(client *Client, machine string) (*QrLogin, error) {
	if client == nil {
		return nil, errors.New("扫码登录不可用：上游客户端未初始化")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return &QrLogin{client: client, code: hex.EncodeToString(b[:]), mach: machine}, nil
}

// State 会话标识（面板轮询用它当 id）。
func (q *QrLogin) State() string { return q.code }

// QRCode 原始随机码（服务端关联用，别直接展示给用户）。
func (q *QrLogin) QRCode() string { return q.code }

// QRContent 二维码里要编的 URL（微信扫这个页面完成授权）。
func (q *QrLogin) QRContent() string {
	query := url.Values{}
	query.Set("code", q.code)
	query.Set("appname", qrAppName)
	return q.client.originBase() + "/login/mp?" + query.Encode()
}

// Poll 查一次扫码状态。
//
// 任何异常都降级为「没完成」：这是 2 秒一次的循环，偶发网络抖动不该中断登录；
// 而把未知状态当 success 会拿到空票据卡死，当 canceled 会无故刷新用户正在扫的码。
// success 但缺 access_token 同样按未完成处理（上游偶发只回状态不回票）。
func (q *QrLogin) Poll(_ context.Context) (*auth.Auth, bool, error) {
	resp, err := q.client.doJSON(http.MethodPost, q.client.authBase()+QrCodePath,
		map[string]string{"qrcode_code": q.code}, q.client.clientIDHeaders())
	if err != nil {
		return nil, false, nil
	}
	data, err := payloadOf(resp, "扫码登录状态")
	if err != nil {
		return nil, false, nil
	}
	raw := text(data, "status")
	q.setStatus(raw)
	switch raw {
	case "canceled":
		return nil, false, ErrQrCanceled
	case "success":
		access := stripBearerToken(text(data, "access_token", "accessToken"))
		refresh := stripBearerToken(text(data, "refresh_token", "refreshToken"))
		if access == "" {
			return nil, false, nil // 缺票的 success：当没完成，继续轮
		}
		creds := &auth.Auth{Data: auth.Data{
			AccessToken:  access,
			RefreshToken: refresh,
			ExpiresAt:    auth.JWTExpiry(access),
			Nickname:     text(data, "office_org_name", "office_identity"),
		}}
		// uid/昵称以 user_info 为准（扫码响应里没有稳定的 office 用户 id）。
		if uid, nick, _, uerr := q.client.GetUserInfo(creds); uerr == nil {
			if uid != "" {
				creds.UID = uid
			}
			if nick != "" {
				creds.Nickname = nick
			}
		}
		if creds.UID == "" {
			return nil, false, errors.New("扫码登录成功但取不到用户标识，请重试")
		}
		if creds.Nickname == "" {
			creds.Nickname = creds.UID
		}
		if q.mach != "" {
			if err := creds.CompleteIdentity(q.mach); err != nil {
				return nil, false, err
			}
		}
		return creds, true, nil
	}
	// pending / logging / 未知状态：都算「还没完成」。
	return nil, false, nil
}

// ErrQrCanceled 用户取消或二维码已失效。
var ErrQrCanceled = errors.New("二维码已失效或已取消，请刷新后重新扫码")

// QrContent 由面板用它渲染二维码（loginQrImage）：{origin}/login/mp?code=<code>&appname=…
func (c *Client) QrContent(code string) string {
	query := url.Values{}
	query.Set("code", code)
	query.Set("appname", qrAppName)
	return c.originBase() + "/login/mp?" + query.Encode()
}

// QrLoginTTL 一个扫码会话的最长寿命（上游二维码本身约 2 分钟有效，这里留足重试余量）。
const QrLoginTTL = 5 * time.Minute

// QrStatusName 把上游状态翻译成面板文案（未知按 pending 处理，与 Poll 同口径）。
func QrStatusName(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "logging":
		return "已扫码，请在手机上确认"
	case "canceled":
		return "已取消或过期"
	case "success":
		return "已授权"
	default:
		return "等待扫码"
	}
}
