package panel

const appJS = `(function () {
  var key = new URLSearchParams(location.search).get("key") || localStorage.getItem("xhx_panel_key") || "";
  if (key) localStorage.setItem("xhx_panel_key", key);
  var loginState = "";

  function api(path, opts) {
    opts = opts || {};
    opts.headers = Object.assign({ "Content-Type": "application/json" }, opts.headers || {});
    if (key) opts.headers["Authorization"] = "Bearer " + key;
    return fetch(path, opts).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (body) {
        if (!r.ok) throw new Error(body.error || ("HTTP " + r.status));
        return body;
      });
    });
  }
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function fmtNum(v) { return v == null || v < 0 ? "—" : Number(v).toLocaleString(); }
  function fmtTok(v) {
    if (!v) return "—";
    if (v >= 1e6) return (v / 1e6).toFixed(1) + "M";
    if (v >= 1000) return (v / 1000).toFixed(1) + "k";
    return String(v);
  }
  function fmtTime(s) { return s ? s.replace("T", " ").slice(5, 19) : "—"; }

  function toast(msg, bad) {
    var el = document.getElementById("status");
    el.textContent = msg;
    el.className = bad ? "bad" : "ok";
    clearTimeout(toast._t);
    toast._t = setTimeout(function () { el.textContent = ""; }, 6000);
  }

  window.loadAll = function () {
    api("/panel/api/overview").then(function (d) {
      document.getElementById("ver").textContent = "v" + (d.version || "");
      var accounts = d.accounts || [];
      var avail = accounts.filter(function (a) { return a.enabled && !a.cooling; }).length;
      document.getElementById("c-accounts").textContent = accounts.length;
      document.getElementById("c-avail").textContent = avail;
      document.getElementById("c-total").textContent = (d.stats && d.stats.total) || 0;
      document.getElementById("c-errors").textContent = (d.stats && d.stats.errors) || 0;
      document.getElementById("c-models").textContent = (d.models || []).length;
      renderAccounts(accounts);
      renderModels(d.models || []);
      renderLogs(d.recent || []);
      document.getElementById("status").textContent = "已连接";
      document.getElementById("status").className = "tag ok";
    }).catch(function (e) {
      document.getElementById("status").textContent = "连接失败: " + e.message;
      document.getElementById("status").className = "tag bad";
      // 没带 key 直接打开 /panel/ 时，让人就地输入，不用回地址栏拼参数。
      if (/密钥/.test(e.message)) document.getElementById("keybox").style.display = "";
    });
  };

  window.saveKey = function () {
    key = document.getElementById("keyinput").value.trim();
    localStorage.setItem("xhx_panel_key", key);
    document.getElementById("keybox").style.display = "none";
    loadAll();
  };

  function renderAccounts(list) {
    var tb = document.getElementById("accounts");
    if (!list.length) { tb.innerHTML = '<tr><td colspan="8" class="muted">还没有账号，先导入或网页登录</td></tr>'; return; }
    tb.innerHTML = list.map(function (a) {
      var pill = a.cooling ? '<span class="pill cool">冷却</span>'
        : a.enabled ? '<span class="pill on">可用</span>' : '<span class="pill off">停用</span>';
      var acts = '<button onclick="act(\'' + a.uid + '\',\'refresh\')">续期</button> ' +
        '<button onclick="act(\'' + a.uid + '\',\'balance\')">额度</button> ';
      if (a.cooling || !a.enabled) acts += '<button onclick="act(\'' + a.uid + '\',\'enable\')">启用</button> ';
      else acts += '<button onclick="act(\'' + a.uid + '\',\'disable\')">停用</button> ';
      acts += '<button class="d" onclick="removeAccount(\'' + a.uid + '\')">删除</button>';
      return "<tr><td>" + esc(a.name) + "</td><td><code>" + esc(a.uid) + "</code></td><td>" + esc(a.token_tail || "") + "</td>" +
        "<td>" + pill + (a.cooling_until ? ' <span class="muted">至 ' + fmtTime(a.cooling_until) + "</span>" : "") + "</td>" +
        "<td>" + fmtNum(a.credit) + "</td><td>" + esc(a.checked_in || "—") + "</td>" +
        "<td>" + esc(a.reason || "") + "</td><td>" + acts + "</td></tr>";
    }).join("");
  }

  function renderModels(list) {
    var tb = document.getElementById("models");
    if (!list.length) { tb.innerHTML = '<tr><td colspan="5" class="muted">上游目录为空（拿不到目录时 /v1/models 会回退到白名单）</td></tr>'; return; }
    tb.innerHTML = list.map(function (m) {
      return "<tr><td><code>" + esc(m.id) + "</code></td><td>" + esc(m.display_name || "") + "</td><td>" +
        (m.max_input_tokens || "—") + "</td><td>" + (m.max_output_tokens || "—") + "</td><td>" +
        (m.billing_multiplier || "—") + "</td></tr>";
    }).join("");
  }

  function renderLogs(list) {
    var tb = document.getElementById("logs");
    if (!list.length) { tb.innerHTML = '<tr><td colspan="8" class="muted">还没有请求</td></tr>'; return; }
    tb.innerHTML = list.map(function (e) {
      var cls = e.status >= 400 ? "bad" : "ok";
      var tok = fmtTok(e.prompt) + " / " + fmtTok(e.completion);
      return "<tr><td class=\"muted\">" + fmtTime(e.time) + "</td><td>" + esc(e.protocol) + "</td><td><code>" +
        esc(e.model) + "</code></td><td>" + esc(e.account || "—") + "</td><td class=\"" + cls + "\">" + e.status +
        "</td><td>" + (e.duration_ms || 0) + "ms</td><td>" + tok + "</td><td class=\"muted\">" + esc(e.error || "") + "</td></tr>";
    }).join("");
  }

  window.importAccount = function () {
    var raw = document.getElementById("imp-json").value.trim();
    var name = document.getElementById("imp-name").value.trim();
    var payload = {};
    if (raw) payload.json = raw; else payload.name = name;
    if (raw && name) payload.name = name;
    api("/panel/api/accounts/import", { method: "POST", body: JSON.stringify(payload) }).then(function (d) {
      document.getElementById("imp-json").value = "";
      document.getElementById("imp-name").value = "";
      toast(d.warning || "已导入 " + d.uid, !!d.warning);
      loadAll();
    }).catch(function (e) { toast(e.message, true); });
  };

  window.act = function (uid, what) {
    var path = "/panel/api/accounts/" + encodeURIComponent(uid) + "/" + what;
    api(path, { method: "POST", body: "{}" }).then(function (d) {
      toast(what + " 完成" + (d.balance ? "，额度 " + fmtNum(d.balance.available) : ""));
      loadAll();
    }).catch(function (e) { toast(e.message, true); });
  };

  window.removeAccount = function (uid) {
    if (!confirm("删除账号 " + uid + "？账号文件会一并删除。")) return;
    api("/panel/api/accounts/" + encodeURIComponent(uid) + "/remove", { method: "POST", body: "{}" })
      .then(function () { toast("已删除"); loadAll(); })
      .catch(function (e) { toast(e.message, true); });
  };

  window.checkinAll = function () {
    api("/panel/api/checkin_all", { method: "POST", body: "{}" }).then(function () { toast("签到完成"); loadAll(); })
      .catch(function (e) { toast(e.message, true); });
  };
  window.balanceAll = function () {
    api("/panel/api/balance_all", { method: "POST", body: "{}" }).then(function () { toast("额度已刷新"); loadAll(); })
      .catch(function (e) { toast(e.message, true); });
  };

  window.openLogin = function () {
    document.getElementById("login-modal").classList.add("show");
    api("/panel/api/login/start", { method: "POST", body: "{}" }).then(function (d) {
      loginState = d.state;
      document.getElementById("login-open").onclick = function () { window.open(d.url, "_blank"); };
      document.getElementById("login-state").textContent = "state " + d.state.slice(0, 8) + "…（10 分钟内有效）";
    }).catch(function (e) { toast(e.message, true); });
  };
  window.openAuth = function () {};
  window.closeLogin = function () { document.getElementById("login-modal").classList.remove("show"); };
  window.exchangeLogin = function () {
    var url = document.getElementById("login-url").value.trim();
    var name = document.getElementById("login-name").value.trim();
    api("/panel/api/login/exchange", { method: "POST", body: JSON.stringify({ url: url, state: loginState, name: name }) })
      .then(function (d) { closeLogin(); toast("已登录 " + (d.name || d.uid)); loadAll(); })
      .catch(function (e) { toast(e.message, true); });
  };

  loadAll();
  setInterval(loadAll, 15000);
})();`
