package panel

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>小浣熊反代面板</title>
<style>
:root{--bg:#0f1115;--card:#171a21;--line:#262b36;--fg:#e7ecf3;--dim:#8b95a7;--acc:#5b9cff;--ok:#3ecf8e;--warn:#f2b544;--bad:#f26d6d}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
header{display:flex;align-items:center;gap:12px;padding:16px 22px;border-bottom:1px solid var(--line);position:sticky;top:0;background:var(--bg);z-index:5}
header h1{font-size:16px;margin:0;font-weight:600}
header .tag{color:var(--dim);font-size:12px}
header .sp{flex:1}
main{padding:18px 22px;max-width:1240px;margin:0 auto;display:grid;gap:16px}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:12px 14px}
.card .k{color:var(--dim);font-size:12px}
.card .v{font-size:22px;font-weight:600;margin-top:2px}
section{background:var(--card);border:1px solid var(--line);border-radius:10px;overflow:hidden}
section>h2{font-size:13px;margin:0;padding:11px 14px;border-bottom:1px solid var(--line);color:var(--dim);font-weight:600;display:flex;gap:8px;align-items:center}
section>h2 .sp{flex:1}
.pad{padding:12px 14px}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{text-align:left;padding:9px 12px;border-bottom:1px solid var(--line);white-space:nowrap}
th{color:var(--dim);font-weight:500;font-size:12px}
tr:last-child td{border-bottom:0}
button{background:#232937;color:var(--fg);border:1px solid var(--line);border-radius:7px;padding:5px 10px;font-size:12px;cursor:pointer}
button:hover{border-color:var(--acc)}
button.p{background:var(--acc);border-color:var(--acc);color:#06101f;font-weight:600}
button.d{color:var(--bad)}
input,textarea,select{background:#0d1016;color:var(--fg);border:1px solid var(--line);border-radius:7px;padding:7px 9px;font:inherit;font-size:13px;width:100%}
textarea{min-height:84px;resize:vertical;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}
.row{display:flex;gap:8px;flex-wrap:wrap;align-items:center}
.row>*{flex:0 0 auto}
.grow{flex:1 1 240px}
.muted{color:var(--dim)}
.ok{color:var(--ok)}.warn{color:var(--warn)}.bad{color:var(--bad)}
.pill{display:inline-block;padding:1px 8px;border-radius:99px;font-size:11px;border:1px solid var(--line)}
.pill.on{color:var(--ok);border-color:#1f4d3a;background:#12241c}
.pill.off{color:var(--dim)}
.pill.cool{color:var(--warn);border-color:#4d3f1f;background:#241f12}
.hint{color:var(--dim);font-size:12px;margin:6px 0 0}
.modal{position:fixed;inset:0;background:#000a;display:none;align-items:center;justify-content:center;padding:20px}
.modal.show{display:flex}
.modal .box{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:16px;max-width:620px;width:100%}
code{background:#0d1016;border:1px solid var(--line);border-radius:5px;padding:1px 5px;font-size:12px}
</style>
</head>
<body>
<header>
  <h1>小浣熊反代面板</h1>
  <span class="tag" id="ver"></span>
  <span class="sp"></span>
  <span class="tag" id="status">加载中…</span>
  <span id="keybox" style="display:none"><input id="keyinput" placeholder="面板密钥" style="width:170px"><button onclick="saveKey()">保存</button></span>
  <button onclick="loadAll()">刷新</button>
</header>
<main>
  <div class="cards">
    <div class="card"><div class="k">账号</div><div class="v" id="c-accounts">–</div></div>
    <div class="card"><div class="k">可用</div><div class="v" id="c-avail">–</div></div>
    <div class="card"><div class="k">请求总数</div><div class="v" id="c-total">–</div></div>
    <div class="card"><div class="k">错误</div><div class="v" id="c-errors">–</div></div>
    <div class="card"><div class="k">模型</div><div class="v" id="c-models">–</div></div>
  </div>

  <section>
    <h2>账号 <span class="sp"></span>
      <button onclick="checkinAll()">全部签到</button>
      <button onclick="balanceAll()">刷新额度</button>
      <button class="p" onclick="openLogin()">网页登录</button>
    </h2>
    <div style="overflow:auto">
      <table>
        <thead><tr><th>名称</th><th>UID</th><th>Token</th><th>状态</th><th>额度</th><th>签到</th><th>最近错误</th><th></th></tr></thead>
        <tbody id="accounts"></tbody>
      </table>
    </div>
    <div class="pad">
      <div class="row">
        <input class="grow" id="imp-name" placeholder="备注名（可选）">
      </div>
      <textarea id="imp-json" placeholder='粘贴账号 JSON：{"access_token":"eyJ…","refresh_token":"eyJ…","name":"小浣熊-1"}'></textarea>
      <p class="hint">token 可在浏览器登录 xiaohuanxiong.com 后，从 localStorage / 网络请求里取 <code>access_token</code> 与 <code>refresh_token</code>。只有 access_token 也能用，但过期后无法自动续期。</p>
      <div class="row" style="margin-top:8px"><button class="p" onclick="importAccount()">导入账号</button></div>
    </div>
  </section>

  <section>
    <h2>模型目录</h2>
    <div style="overflow:auto"><table><thead><tr><th>ID</th><th>名称</th><th>上下文</th><th>输出上限</th><th>倍率</th></tr></thead><tbody id="models"></tbody></table></div>
  </section>

  <section>
    <h2>最近请求</h2>
    <div style="overflow:auto"><table><thead><tr><th>时间</th><th>协议</th><th>模型</th><th>账号</th><th>状态</th><th>耗时</th><th>tokens</th><th>错误</th></tr></thead><tbody id="logs"></tbody></table></div>
  </section>
</main>

<div class="modal" id="login-modal">
  <div class="box">
    <h2 style="margin:0 0 10px;font-size:14px">网页登录小浣熊</h2>
    <p class="hint">1. 点下面的按钮打开授权页，用要接入的账号登录；2. 浏览器会跳到 <code>office-raccoon://auth/callback?...</code>（页面可能提示无法打开，正常）；3. 把地址栏整条链接复制回来粘贴。</p>
    <div class="row" style="margin:10px 0">
      <button class="p" id="login-open" onclick="openAuth()">打开授权页</button>
      <span class="muted" id="login-state"></span>
    </div>
    <input id="login-url" placeholder="office-raccoon://auth/callback?code=…&state=…">
    <input id="login-name" placeholder="备注名（可选）" style="margin-top:8px">
    <div class="row" style="margin-top:10px">
      <button class="p" onclick="exchangeLogin()">完成登录</button>
      <button onclick="closeLogin()">取消</button>
    </div>
  </div>
</div>

<script src="/panel/app.js"></script>
</body>
</html>`
