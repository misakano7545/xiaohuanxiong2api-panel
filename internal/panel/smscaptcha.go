// smscaptcha.go 阿里云滑块校验的**独立辅助页**。
//
// 为什么单独开一页，而不是在主面板里跑：
//
//  1. 主面板的 CSP 是 script-src 'self'，这是它挡住 XSS 注入的最后一层。阿里云滑块脚本
//     必须从 o.alicdn.com / g.alicdn.com 加载——把这两个源加进**主面板**等于给每次面板
//     会话都开一个远程代码执行入口（面板里存着 API key 和全部账号 token）。
//  2. 滑块组件是第三方 widget：它会往 document 里塞固定定位的 iframe/遮罩，实测在本面板
//     的 .veil 弹窗里初始化成功（拿到实例）却渲染不出任何节点，弹窗层级与它的样式直接冲突。
//     放在独立顶层页里没有这些干扰。
//  3. 这一页不读 localStorage、不调任何 /panel/api、拿不到 API key：它唯一的输出是
//     postMessage 回主窗口的一串 captcha_param。万一第三方脚本被投毒，它能碰到的只有这一页。
//
// 主面板仍然只放行同源脚本，这里放行阿里云那两组域。
package panel

import (
	"net/http"
)

// 阿里云验证码的常量取自官方客户端渲染层（与抓包一致）。
const (
	smsCaptchaScript = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
	smsCaptchaScene  = "1pkmy0x3"
	smsCaptchaPrefix = "hk1r5l"
)

// 只对这一页放宽：外部脚本、样式、图片、校验请求。
const smsCaptchaCSP = "default-src 'none'; " +
	"script-src https://*.alicdn.com 'unsafe-inline'; " +
	"style-src 'unsafe-inline' https://*.alicdn.com; " +
	"connect-src https://*.alicdn.com https://*.aliyuncs.com; " +
	"img-src data: https://*.alicdn.com https://*.aliyuncs.com; " +
	"frame-src https://*.aliyuncs.com https://*.alicdn.com; " +
	"form-action 'none'; base-uri 'none'"

// smsCaptchaPage 渲染辅助页：人机校验通过后把 captcha_param postMessage 回主窗口。
func (p *Panel) smsCaptchaPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", smsCaptchaCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(smsCaptchaHTML))
}

// smsCaptchaHTML 辅助页本体。刻意做成最小：一段样式 + 一个挂载点 + 初始化脚本。
// 校验通过 → postMessage({type:'xhx-sms-captcha', param}) → 关闭自己。
const smsCaptchaHTML = `<!doctype html><html lang="zh-CN"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>人机校验 · 短信登录</title>
<style>
  :root { color-scheme: light dark; }
  body { margin:0; padding:28px 26px 34px; font:14px/1.6 system-ui,-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;
         background:#fff; color:#1b1b1f; }
  h1 { font-size:15px; margin:0 0 6px; font-weight:600; }
  p  { margin:0 0 18px; color:#5b5b66; font-size:13px; }
  #cap { min-height:64px; }
  .foot { margin-top:18px; font-size:12px; color:#8a8a94; }
  @media (prefers-color-scheme: dark) {
    body { background:#17181c; color:#e8e8ee; } p, .foot { color:#9a9aa6; }
  }
</style></head><body>
<h1>拖动滑块完成人机校验</h1>
<p>通过后会自动把结果带回面板，并关闭本页。</p>
<div id="cap"></div>
<div class="foot" id="st">正在加载校验组件…</div>
<script>
  // 必须在加载 AliyunCaptcha.js **之前**声明：验证码 2.0 从这两个全局量读区域与站点前缀，
  // 缺了它会静默失败——实测表现为「initAliyunCaptcha 正常返回实例、但一个节点都不渲染」，
  // 控制台只有一句组件内部的 network error。
  window.AliyunCaptchaConfig = { region: 'cn', prefix: '` + smsCaptchaPrefix + `' };
</script>
<script src="` + smsCaptchaScript + `"></script>
<script>
(function () {
  'use strict';
  var st = document.getElementById('st');
  function fail(msg) { st.textContent = msg; }
  function back(param) {
    try {
      if (window.opener && !window.opener.closed) {
        window.opener.postMessage({ type: 'xhx-sms-captcha', param: String(param || '') }, location.origin);
        st.textContent = '校验通过，已带回面板，本页自动关闭';
        setTimeout(function () { window.close(); }, 600);
      } else {
        st.textContent = '校验通过，但面板窗口已关闭，请回到面板重新发送';
      }
    } catch (e) { fail('回传失败：' + e.message); }
  }
  if (typeof window.initAliyunCaptcha !== 'function') {
    fail('校验组件未加载（网络或浏览器策略拦截），请重试或改用微信扫码登录');
    return;
  }
  try {
    window.initAliyunCaptcha({
      SceneId: '` + smsCaptchaScene + `',
      prefix: '` + smsCaptchaPrefix + `',
      mode: 'embed',
      element: '#cap',
      captchaVerifyCallback: function (param) {
        // 校验通过：把 param 交回主窗口，然后让组件收尾。
        back(param);
        return { captchaResult: true, bizResult: true };
      },
      onBizResultCallback: function () { return null; },
      getInstance: function () { return null; },
      language: 'cn'
    });
    st.textContent = '请拖动滑块';
  } catch (e) {
    fail('校验组件初始化失败：' + e.message);
  }
})();
</script>
</body></html>`
