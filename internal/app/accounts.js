Papa.page("accounts", function (root) {
  "use strict";
  var u = window.PapaRuntime, el = u.el, button = u.button;
  var dead = false, writing = false, rebuilding = false, dataGeneration = 0, panelGeneration = 0, processVisible = false, data, drawer, process, timer, remoteTimer, lease = "", controlID = "", remoteBusy = false, remoteQueue = [], remoteImage, remotePages;
  var status = el("p", "正在读取账号资料…", "rt-status"), content = el("div"), bar = el("div", undefined, "rt-toolbar");
  status.setAttribute("role", "status");
  var style = el("style");
  style.textContent = ".ac-flow{border-left:4px solid var(--primary,#ffd83d);padding:12px 18px;margin:20px 0}.ac-fields{display:grid;gap:14px;margin:20px 0}.ac-fields label{display:grid;gap:6px}.ac-fields input,.ac-fields select,.ac-fields textarea{padding:9px;width:100%;box-sizing:border-box}.ac-identity{background:var(--muted-fill,#f7f7f5);padding:18px;white-space:pre-wrap;overflow-wrap:anywhere}.ac-remote{width:100%;height:auto;display:block;outline:2px solid var(--border-color,#222);cursor:crosshair;touch-action:none}.ac-remote:focus{outline:3px solid var(--primary,#ffd83d)}.ac-ops{display:flex;gap:8px;flex-wrap:wrap}.ac-wide{width:min(1100px,100%)}";
  bar.append(el("h2", "账号管理"), button("新增账号", function () { edit(); }), button("刷新", refresh));
  root.append(style, bar, status, content);
  function current() { return !dead; }
  function setWriting(value) { writing = value; root.querySelectorAll("button,input,select,textarea").forEach(function (control) { if (value) { control.dataset.acWasDisabled = control.disabled ? "1" : "0"; control.disabled = true; } else if (control.dataset.acWasDisabled !== undefined) { control.disabled = control.dataset.acWasDisabled === "1"; delete control.dataset.acWasDisabled; } }); }
  async function request(path, body) { return u.request(path, body ? { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) } : null, current); }
  function notice(e) { if (!dead) status.textContent = e.message || String(e); }
  async function refresh() {
    var generation = ++dataGeneration;
    try { var latest = await request("/api/accounts"); if (dead || generation !== dataGeneration) return; data = latest; status.textContent = latest.configured ? "先保存资料，再完成登录并核对真实身份。验证通过后可加入同站账号池。" : latest.message; render(); } catch (e) { if (generation === dataGeneration) notice(e); }
  }
  function render() {
    content.replaceChildren(); if (!data) return;
    if (!data.providers.length) { content.append(el("p", "尚无站点声明登录方式。请由业务模块注册适配器，并显式提供加密存储密钥。")); return; }
    var rows = (data.profiles || []).map(function (p) {
      var ops = el("div", undefined, "ac-ops"); ops.append(button(p.state === "ready" ? "重新登录" : "登录", function () { login(p); }), button("资料", function () { edit(p); }));
      if (p.state === "ready") ops.append(button("加入账号池", function () { join(p); }));
      return [p.key.site, p.label + " · " + p.key.id, p.state === "ready" ? "已核验" : "待登录", p.identity ? p.identity.display : "尚未识别", p.identity ? [p.identity.http ? "HTTP" : "", p.identity.browser ? "浏览器" : ""].filter(Boolean).join(" / ") : "—", u.date(p.verified_at), ops];
    });
    content.append(u.table(["站点", "账号", "接入状态", "真实身份", "已核验能力", "最近核验", "操作"], rows));
  }
  function close() { if (writing) return false; panelGeneration++; if (!rebuilding) { processVisible = false; stopRemote(true); } else { clearTimeout(remoteTimer); remoteImage = null; remotePages = null; remoteQueue = []; } if (drawer) drawer.remove(); drawer = null; return true; }
  function open(title, wide) { if (!close()) return null; drawer = el("section", undefined, "rt-drawer" + (wide ? " ac-wide" : "")); drawer.setAttribute("role", "dialog"); drawer.setAttribute("aria-label", title); drawer.append(el("h2", title), button("关闭", close)); root.append(drawer); return drawer; }
  function field(form, label, type, value) { var row = el("label", label), input = el(type === "textarea" ? "textarea" : "input"); if (type !== "textarea") input.type = type || "text"; input.value = value || ""; input.autocomplete = "off"; row.append(input); form.append(row); return input; }
  function edit(p) {
    if (!data || !data.configured || !data.providers.length) return;
    var box = open(p ? "账号资料" : "新增账号"); if (!box) return; var form = el("div", undefined, "ac-fields"), select = el("select"), label = el("label", "站点");
    data.providers.forEach(function (d) { var o = el("option", d.label + " · " + d.site); o.value = d.site; select.append(o); }); if (p) select.value = p.key.site; select.disabled = !!p; label.append(select); form.append(label);
    var id = field(form, "稳定账号 ID", "text", p && p.key.id), name = field(form, "显示名称", "text", p && p.label), expected = field(form, "预期真实账号 ID（选填）", "text", p && p.expected_subject); id.disabled = !!p; expected.disabled = !!(p && p.state === "ready");
    box.append(form, el("p", "账号 ID 不随密码、Cookie 或 Token 改变。资料草稿不会进入采集池。", "rt-muted"), button("保存资料", async function () {
      if (writing) return; setWriting(true);
      try { var saved = await request("/api/accounts", { action: "draft", revision: p ? p.revision : 0, profile: { key: { site: select.value, id: id.value }, label: name.value, expected_subject: expected.value } }); setWriting(false); close(); await refresh(); if (!p) login(saved); } catch (e) { setWriting(false); notice(e); }
    }));
  }
  function login(p) {
    var provider = data.providers.find(function (v) { return v.site === p.key.site; }); if (!provider) return;
    var box = open("登录 · " + p.label); if (!box) return; var select = el("select"), fields = el("div", undefined, "ac-fields"), values = {}, remember = el("input"); remember.type = "checkbox";
    provider.methods.forEach(function (m) { var o = el("option", m.label); o.value = m.id; select.append(o); });
    var rememberLabel = el("label", " 加密保存本方式允许保存的字段（可选）"); rememberLabel.prepend(remember);
    function build() { fields.replaceChildren(); values = {}; var m = provider.methods.find(function (v) { return v.id === select.value; }); (m.fields || []).forEach(function (f) { values[f.name] = field(fields, f.label + (f.required ? " *" : ""), m.kind === "import" ? "textarea" : f.secret ? "password" : "text"); }); rememberLabel.hidden = !(m.fields || []).some(function (f) { return f.persist; }); remember.checked = false; }
    select.onchange = build; build(); box.append(select, fields, rememberLabel, el("p", "登录更新使用独立候选；失败时保留原来可用的登录态。", "rt-muted"), button("开始登录", async function () {
      if (writing) return; var input = {}; Object.keys(values).forEach(function (k) { input[k] = values[k].value; }); setWriting(true);
      try { process = await request("/api/accounts", { action: "start", key: p.key, revision: p.revision, method: select.value, input: input, remember: remember.checked }); Object.keys(values).forEach(function (k) { values[k].value = ""; }); setWriting(false); sessionStorage.setItem("papa-account-login", process.id); showProcess(true); schedule(); } catch (e) { setWriting(false); notice(e); }
    }));
  }
  var names = { running: "正在处理", waiting_input: "等待输入", waiting_scan: "等待扫码", remote: "等待页面操作", confirm_identity: "请核对真实身份", succeeded: "账号已保存并生效", failed: "登录未完成", expired: "登录已过期", cancelled: "登录已取消" };
  var failures = { identity_mismatch: "真实账号与预期绑定不一致，候选已拒绝。", identity_verification_failed: "未能确认账号身份或权限。", candidate_publish_failed: "凭据发布未完成；请核对资料版本及原账号状态。", remote_disconnected: "远程控制连接已断开并回收。", login_expired: "登录过程已过期，请重新发起。", login_budget_wait: "账号登录间隔尚未结束，请稍后再试。", health_restore_pending: "凭据已更新，恢复状态待保存。请到账号池核对；人工停用状态保持不变。" };
  function terminal() { return process && ["succeeded", "failed", "expired", "cancelled"].includes(process.state); }
  function showProcess(force) {
    if (!process || dead || (!force && !processVisible)) return; if (!force && drawer && drawer.dataset.process === process.id && drawer.dataset.revision === String(process.revision)) return;
    rebuilding = true; var box = open("登录进度", process.state === "remote"); rebuilding = false; if (!box) return; processVisible = true; box.dataset.process = process.id; box.dataset.revision = String(process.revision);
    box.append(el("p", names[process.state] || process.state, "ac-flow"), el("p", "账号 " + process.key.site + " / " + process.key.id + " · 到期 " + u.date(process.expires_at), "rt-muted"));
    if (process.failure) box.append(el("p", failures[process.failure] || "站点登录或检查未完成，请重新核对后发起。", "rt-status"));
    if (process.image_data_url) { var qr = el("img"); qr.src = process.image_data_url; qr.alt = "站点登录二维码"; qr.style.maxWidth = "300px"; box.append(qr); }
    if (process.state === "confirm_identity") { var i = process.identity; box.append(el("div", "识别到的账号：" + i.display + "\n真实 ID：" + i.subject + "\n权限：" + (i.scopes || []).join("、") + "\n能力：" + [i.http ? "HTTP" : "", i.browser ? "浏览器" : ""].filter(Boolean).join(" / "), "ac-identity"), button("身份正确，保存并启用", function () { act("confirm", {}); })); }
    if (["waiting_input", "waiting_scan", "remote"].includes(process.state)) {
      var form = el("div", undefined, "ac-fields"), inputs = {}; (process.fields || []).forEach(function (f) { inputs[f.name] = field(form, f.label, f.secret ? "password" : "text"); }); box.append(form);
      if (process.state === "remote") remotePanel(box);
      box.append(button(process.state === "waiting_scan" ? "检查扫码结果" : process.state === "remote" ? "已完成页面登录，检查身份" : "提交本次输入", function () { var v = {}; Object.keys(inputs).forEach(function (k) { v[k] = inputs[k].value; }); act("continue", v); }));
    }
    if (!terminal()) box.append(button("取消登录", function () { act("cancel", {}); })); else { sessionStorage.removeItem("papa-account-login"); sessionStorage.removeItem("papa-account-control"); if (process.state === "succeeded") box.append(el("p", "可从账号列表加入同站账号池。现有健康账号不会被自动替换。")); refresh(); }
  }
  async function act(action, input) { if (writing || !process) return; setWriting(true); try { var next = await request("/api/accounts", { action: action, id: process.id, revision: process.revision, input: input }); setWriting(false); process = next; showProcess(true); schedule(); } catch (e) { setWriting(false); notice(e); await poll(); } }
  function schedule() { clearTimeout(timer); if (!dead && process && !terminal()) timer = setTimeout(poll, 1200); }
  async function poll() { if (!process || dead) return; var id = process.id; try { var next = await request("/api/accounts?process=" + encodeURIComponent(id)); if (dead || !process || process.id !== id) return; if (next.revision >= (process.revision || 0)) process = next; if (!writing) showProcess(false); } catch (e) { if (!process || process.id !== id) return; notice(e); if ([403, 410].includes(e.status)) { sessionStorage.removeItem("papa-account-login"); process = null; stopRemote(false); } } schedule(); }
  async function join(p) {
    try { var d = await request("/api/account-pools"), pools = d.pools.filter(function (v) { return v.key.site === p.key.site; }), box = open("加入账号池 · " + p.label); if (!box) return; var select = el("select"); pools.forEach(function (v, i) { var o = el("option", v.key.name + (v.members.includes(p.key.id) ? "（已加入）" : "")); o.value = String(i); select.append(o); }); box.append(el("p", "只列出同站账号池。请按实际权限分池；加入成员不会抢占当前健康账号。"), select); if (!pools.length) { box.append(el("p", "尚无同站账号池，请到“账号池”页面创建。")); return; }
      box.append(button("加入所选池", async function () { if (writing) return; var v = pools[Number(select.value)]; if (v.members.includes(p.key.id)) { status.textContent = "账号已经在池中。"; return; } setWriting(true); var cfg = {}; ["key", "members", "transport", "disabled", "verify_attempts", "switch_limit", "cooldown", "standby_target", "standby_ttl"].forEach(function (k) { if (v[k] !== undefined) cfg[k] = v[k]; }); cfg.members = v.members.concat(p.key.id); try { await request("/api/account-pools", { action: "save", revision: v.revision, config: cfg }); setWriting(false); close(); status.textContent = "已加入账号池。"; } catch (e) { setWriting(false); notice(e); } }));
    } catch (e) { notice(e); }
  }
  function stopRemote(release) { clearTimeout(remoteTimer); remoteQueue = []; if (release && lease && controlID) request("/api/accounts/remote", { id: controlID, action: "release", lease: lease }).catch(function () {}); if (release) sessionStorage.removeItem("papa-account-control"); lease = ""; controlID = ""; remoteImage = null; remotePages = null; }
  function remotePanel(box) {
    remotePages = el("select"); remotePages.setAttribute("aria-label", "登录页面"); remotePages.onchange = function () { remoteCommand("select", { page: remotePages.value }); };
    remoteImage = el("img", undefined, "ac-remote"); remoteImage.alt = "服务器上的受控登录页面"; remoteImage.tabIndex = 0;
    remoteImage.onclick = function (e) { var rect = remoteImage.getBoundingClientRect(); remoteImage.focus(); remoteCommand("click", { x: (e.clientX - rect.left) / rect.width, y: (e.clientY - rect.top) / rect.height }); };
    remoteImage.onkeydown = function (e) { if (["Enter", "Tab", "Backspace", "Escape", "ArrowLeft", "ArrowUp", "ArrowRight", "ArrowDown", "Delete"].includes(e.key)) { e.preventDefault(); e.stopPropagation(); remoteCommand("key", { key: e.key }); } };
    remoteImage.onwheel = function (e) { e.preventDefault(); var rect = remoteImage.getBoundingClientRect(); remoteCommand("scroll", { x: Math.max(0, Math.min(.999, (e.clientX - rect.left) / rect.width)), y: Math.max(0, Math.min(.999, (e.clientY - rect.top) / rect.height)), delta_y: Math.max(-720, Math.min(720, e.deltaY)) }); };
    var form = el("div", undefined, "ac-fields"), input = field(form, "向当前焦点输入文字", "password"); box.append(el("p", "点击画面定位，再输入文字。页面选择仅限本站声明的登录流程；关闭控制面板会结束本次远程登录。", "rt-muted"), remotePages, remoteImage, form, button("发送文字", function () { var value = input.value; input.value = ""; remoteCommand("text", { text: value }); })); remoteCommand("claim", {});
  }
  async function remoteCommand(action, values) {
    if (dead || writing || !process || !remoteImage) return; var id = process.id, generation = panelGeneration;
    if (remoteBusy) { if (action !== "frame" && remoteQueue.length < 8) remoteQueue.push([action, values, id, generation]); return; } remoteBusy = true;
    function samePanel() { return !dead && remoteImage && process && process.id === id && panelGeneration === generation; }
    try { var frame = await request("/api/accounts/remote", Object.assign({ id: id, action: action, lease: controlID === id ? lease : "" }, values)); if (!samePanel()) return; lease = frame.lease; controlID = id; sessionStorage.setItem("papa-account-control", JSON.stringify({ id: id, lease: lease })); if (frame.image_data_url) remoteImage.src = frame.image_data_url; if (frame.pages) { remotePages.replaceChildren(); frame.pages.forEach(function (p) { var o = el("option", p.label); o.value = p.id; remotePages.append(o); }); remotePages.value = frame.page; } }
    catch (e) { if (samePanel()) { remoteQueue = []; if (e.status !== 429 || action !== "frame") notice(e); if ([403, 410].includes(e.status)) stopRemote(false); } }
    finally { remoteBusy = false; if (!dead && remoteImage && process) { remoteQueue = remoteQueue.filter(function (q) { return q[2] === process.id && q[3] === panelGeneration; }); if (remoteQueue.length) { clearTimeout(remoteTimer); var next = remoteQueue.shift(); remoteTimer = setTimeout(function () { remoteCommand(next[0], next[1]); }, 20); } else if (samePanel() && lease) { clearTimeout(remoteTimer); remoteTimer = setTimeout(function () { remoteCommand("frame", {}); }, 1000); } } }
  }
  refresh(); var remembered = sessionStorage.getItem("papa-account-login"); if (remembered) { process = { id: remembered }; processVisible = true; try { var control = JSON.parse(sessionStorage.getItem("papa-account-control") || "null"); if (control && control.id === remembered) { lease = control.lease; controlID = remembered; } } catch (_) {} poll(); }
  var removeKeyboard = u.dialogKeyboard(function () { return drawer; }, close);
  return function () { stopRemote(true); dead = true; clearTimeout(timer); clearTimeout(remoteTimer); if (removeKeyboard) removeKeyboard(); if (drawer) drawer.remove(); };
});
