// smslogin.go 面板侧短信登录端点：发验证码 / 用验证码登录。
//
// 与扫码登录的分工：扫码不需要任何输入，但要求有微信；短信登录只需要手机号 + 验证码，
// 代价是每发一次要用阿里云滑块过一次人机校验（上游强制，见 upstream/smslogin.go）。
//
// 防滥用：验证码只能发给用户自己填的号，且加冷却——面板是自架服务，不能变成谁都能拿来
// 打别家号码的短信轰炸器。冷却按号码算（不是按 IP：同一出口 IP 可能有多个真实用户）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

const (
	smsCooldown      = 60 * time.Second // 同一号码两次下发的最小间隔
	smsHourlyBudget  = 5                // 同一号码每小时最多下发次数
	smsHourlyWindow  = time.Hour
	smsPhoneMaxChars = 20
)

// smsGuard 下发频控（内存态，进程重启即清空——重启本身就意味着没人能连打）。
type smsGuard struct {
	mu   sync.Mutex
	last map[string]time.Time
	hist map[string][]time.Time
}

func (g *smsGuard) allow(phone string) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.last == nil {
		g.last, g.hist = map[string]time.Time{}, map[string][]time.Time{}
	}
	if last, ok := g.last[phone]; ok {
		if wait := smsCooldown - now.Sub(last); wait > 0 {
			// 用秒数而不是 Go 的 1m0s：面板文案给普通人看，别露内部时长格式。
			secs := int((wait + time.Second - 1) / time.Second)
			return false, "刚刚发过验证码，请 " + itoa(secs) + " 秒后再试"
		}
	}
	kept := g.hist[phone][:0]
	for _, t := range g.hist[phone] {
		if now.Sub(t) < smsHourlyWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= smsHourlyBudget {
		return false, "这个号码一小时内已发送 " + itoa(smsHourlyBudget) + " 次，请稍后再试"
	}
	g.last[phone] = now
	g.hist[phone] = append(kept, now)
	return true, ""
}

// itoa 小工具（只为拼文案，避免为它引 strconv 到热路径以外的依赖面）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// phonePattern 只做形状校验（1 开头 11 位）：区号目前只支持 86（客户端下拉有 852/853/81，
// 但上游对非 86 的号码一律回 100002，无法区分「号码错」与「区号不支持」，故不猜）。
var phonePattern = regexp.MustCompile(`^1\d{10}$`)

// smsSend 下发短信验证码。body: {"phone":"138…","captcha_param":"<阿里云滑块产物>"}
func (p *Panel) smsSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone   string `json:"phone"`
		Captcha string `json:"captcha_param"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	phone := digitsOnly(req.Phone)
	if !phonePattern.MatchString(phone) {
		writeErr(w, http.StatusBadRequest, "请填写 11 位手机号（当前只支持 +86）")
		return
	}
	if len(req.Captcha) > 4096 || len(phone) > smsPhoneMaxChars {
		writeErr(w, http.StatusBadRequest, "参数过长")
		return
	}
	if ok, why := p.sms.allow(phone); !ok {
		writeErr(w, http.StatusTooManyRequests, why)
		return
	}
	if err := p.cfg.Upstream.SendSmsCode(phone, req.Captcha); err != nil {
		log.Printf("panel: 短信下发失败 %s: %v", maskPhone(phone), err)
		// 下发失败不占冷却额度之外的东西，但把冷却退掉：用户没收到码，不该被锁 60 秒。
		p.sms.refund(phone)
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	log.Printf("panel: 已下发短信验证码 %s", maskPhone(phone))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "phone": maskPhone(phone)})
}

// refund 撤回最近一次占用（下发失败时用）。
func (g *smsGuard) refund(phone string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.last, phone)
	if h := g.hist[phone]; len(h) > 0 {
		g.hist[phone] = h[:len(h)-1]
	}
}

// smsVerify 用验证码登录并入池。body: {"phone":"138…","code":"123456"}
func (p *Panel) smsVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	phone := digitsOnly(req.Phone)
	if !phonePattern.MatchString(phone) {
		writeErr(w, http.StatusBadRequest, "请填写 11 位手机号（当前只支持 +86）")
		return
	}
	if len(req.Code) > 16 {
		writeErr(w, http.StatusBadRequest, "验证码过长")
		return
	}
	creds, err := p.cfg.Upstream.LoginWithSms(phone, req.Code)
	if err != nil {
		log.Printf("panel: 短信登录失败 %s: %v", maskPhone(phone), err)
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: 短信登录成功 %s uid=%s", maskPhone(phone), uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "nickname": nick})
}

// digitsOnly 只留数字：用户常带空格/横杠，上游只认纯数字。
func digitsOnly(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// maskPhone 日志里只留前 3 后 4：手机号是个人标识，不该整条落到日志/归档。
func maskPhone(phone string) string {
	if len(phone) < 7 {
		return "***"
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

var _ = upstream.SendSmsPath
