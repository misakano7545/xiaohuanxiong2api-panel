// 面板静态资源与安全响应头。go:embed 打进二进制，无外部构建。
package panel

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

// CSP 与参考面板同级：脚本只允许同源，样式允许内联（色条宽度），禁止被嵌套。
//
// 主面板**不放行任何外部脚本**：短信登录要用的阿里云滑块放在独立的辅助页
// （/panel/sms-captcha，见 smscaptcha.go），那一页有自己的、最小化的 CSP。
// 这样外来的远程 JS 永远进不了持有 API key 与账号 token 的这个文档。
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	// 阿里云滑块要按来源判定场景域名：no-referrer 会让它拿不到来源、校验直接失败
	// （实测表现为脚本内部 "network error" + 滑块 iframe 永远 display:none）。
	// strict-origin-when-cross-origin 只把「协议+主机+端口」发给跨域目标，不带路径。
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(appJS)
}
