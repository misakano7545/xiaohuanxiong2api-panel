package panel

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// 主面板的 CSP 不许出现任何外部脚本源：短信登录要用的阿里云滑块必须待在独立辅助页
// （/panel/sms-captcha）。这条断言是那个架构决定的门闩——把它放宽到主面板等于给持有
// API key 与全部账号 token 的文档开一个远程代码入口。
func TestMainPanelCSPHasNoExternalOrigin(t *testing.T) {
	for _, bad := range []string{"alicdn", "aliyuncs", "http://", "https://"} {
		if strings.Contains(csp, bad) {
			t.Errorf("主面板 CSP 里出现了外部源 %q：%s", bad, csp)
		}
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("主面板 CSP 必须锁死同源脚本：%s", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("主面板 CSP 丢了 frame-ancestors 'none'：%s", csp)
	}
}

// 辅助页有自己的、最小化的 CSP：只放行阿里云那两组域，且不允许被嵌套。
func TestSmsCaptchaPageOwnCSP(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	req := httptest.NewRequest(http.MethodGet, "/panel/sms-captcha", nil)
	req.RemoteAddr = "127.0.0.1:51234"
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("辅助页 status = %d", rec.Code)
	}
	got := rec.Header().Get("Content-Security-Policy")
	if got == "" {
		t.Fatal("辅助页必须自带 CSP（不能继承主面板的严格策略，否则滑块加载不了）")
	}
	if !strings.Contains(got, "script-src") || !strings.Contains(got, "alicdn") {
		t.Errorf("辅助页 CSP 应放行阿里云脚本：%s", got)
	}
	if strings.Contains(got, "'self'") && strings.Contains(got, "script-src 'self'") {
		t.Errorf("辅助页不该依赖同源脚本")
	}
	body := rec.Body.String()
	if strings.Contains(body, "/panel/api/") {
		t.Error("辅助页不该调用任何面板接口（它拿不到密钥，也不该碰账号数据）")
	}
	// 阿里云验证码 2.0 要求先声明全局配置再加载脚本，顺序错了会静默不渲染。
	cfgAt := strings.Index(body, "AliyunCaptchaConfig")
	scriptAt := strings.Index(body, "aliyunCaptcha/AliyunCaptcha.js")
	if cfgAt < 0 || scriptAt < 0 || cfgAt > scriptAt {
		t.Errorf("AliyunCaptchaConfig 必须在加载脚本之前声明（cfg=%d script=%d）", cfgAt, scriptAt)
	}
}

// 手机号加密：AES-128-CFB、NoPadding、Base64(iv‖密文)。这里做自洽校验（解回来等于原号），
// 服务端口径已用错误码实测确认：加密号 → 100006（人机校验），明文号 → 100003（加密参数错）。
func TestEncryptPhoneShape(t *testing.T) {
	raw, err := upstream.EncryptPhone("13800000000")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("不是合法 base64: %v", err)
	}
	if len(blob) != 16+11 {
		t.Fatalf("长度应为 16(IV)+11(号长)，得到 %d", len(blob))
	}
	block, err := aes.NewCipher([]byte(upstream.SmsPhoneCipherSecret()))
	if err != nil {
		t.Fatal(err)
	}
	if len([]byte(upstream.SmsPhoneCipherSecret())) != 16 {
		t.Fatalf("密钥必须 16 字节（AES-128）")
	}
	plain := make([]byte, len(blob)-16)
	cipher.NewCFBDecrypter(block, blob[:16]).XORKeyStream(plain, blob[16:])
	if string(plain) != "13800000000" {
		t.Fatalf("解密回原文失败: %q", plain)
	}
	// 两次加密必须不同（IV 随机）：相同密文等于把同一个号码摊在日志里。
	another, _ := upstream.EncryptPhone("13800000000")
	if another == raw {
		t.Error("同一号码两次加密结果相同——IV 没随机")
	}
}

// 号码形状不对时面板自己拦下，不去打上游（也就不可能替别人发短信）。
func TestSmsSendRejectsBadPhone(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	for _, body := range []string{`{"phone":"12345"}`, `{"phone":""}`, `{"phone":"1234567890123"}`} {
		code, _ := call(t, pn, http.MethodPost, "/panel/api/login/sms/send", body)
		if code != http.StatusBadRequest {
			t.Errorf("body %s 应 400，得到 %d", body, code)
		}
	}
}

// 同一号码 60 秒内不许连发：自架面板不能变成短信轰炸器。
func TestSmsSendRateLimited(t *testing.T) {
	g := &smsGuard{}
	if ok, _ := g.allow("13800000000"); !ok {
		t.Fatal("第一次应当放行")
	}
	if ok, why := g.allow("13800000000"); ok {
		t.Fatal("冷却期内不该再放行")
	} else if !strings.Contains(why, "秒") {
		t.Errorf("冷却提示应带剩余时间：%s", why)
	}
	if ok, _ := g.allow("13900000000"); !ok {
		t.Error("不同号码互不影响")
	}
	// 下发失败要退还额度，否则用户没收到码还被锁 60 秒。
	g.refund("13800000000")
	if ok, _ := g.allow("13800000000"); !ok {
		t.Error("refund 后应能立刻重发")
	}
}
