(function () {
"use strict";
function mountAccounts(root, options) {
  options = options || {};
  var targetPool = null, pendingJoin = null;
  "use strict";
  var u = window.PapaRuntime, el = u.el, button = u.button;
  var dead = false, writing = false, rebuilding = false, dataGeneration = 0, panelGeneration = 0, processVisible = false, data, drawer, process, timer, remoteTimer, lease = "", controlID = "", remoteBusy = false, remoteQueue = [], remoteImage, remotePages;
  var status = el("p", "正在读取账号资料…", "rt-status"), content = el("div"), bar = el("div", undefined, "rt-toolbar");
  status.setAttribute("role", "status");
  var style = el("style");
  style.textContent = ".ac-flow{border-left:4px solid var(--primary,#ffd83d);padding:12px 18px;margin:20px 0}.ac-fields{display:grid;gap:14px;margin:20px 0}.ac-fields label{display:grid;gap:6px}.ac-fields input,.ac-fields select,.ac-fields textarea{padding:9px;width:100%;box-sizing:border-box}.ac-identity{background:var(--muted-fill,#f7f7f5);padding:18px;white-space:pre-wrap;overflow-wrap:anywhere}.ac-remote{width:100%;height:auto;display:block;outline:2px solid var(--border-color,#222);cursor:crosshair;touch-action:none}.ac-remote:focus{outline:3px solid var(--primary,#ffd83d)}.ac-ops{display:flex;gap:8px;flex-wrap:wrap}.ac-wide{width:min(1100px,100%)}";
  var siteFilter = el("select"), selectedSite = sessionStorage.getItem("papa-account-site") || "";
  siteFilter.setAttribute("aria-label", "筛选账号站点");
  siteFilter.onchange = function () { if (!close()) { siteFilter.value = selectedSite; return; } selectedSite = siteFilter.value; sessionStorage.setItem("papa-account-site", selectedSite); render(); };
  bar.append(el("h2", "站点账号"), siteFilter, button("新增账号", function () { edit(); }), button("刷新", refresh));
  if (options.embedded) { bar.hidden = true; status.hidden = true; content.hidden = true; }
  root.append(style, bar, status, content);
  function current() { return !dead; }
  function setWriting(value) { writing = value; root.querySelectorAll("button,input,select,textarea").forEach(function (control) { if (value) { control.dataset.acWasDisabled = control.disabled ? "1" : "0"; control.disabled = true; } else if (control.dataset.acWasDisabled !== undefined) { control.disabled = control.dataset.acWasDisabled === "1"; delete control.dataset.acWasDisabled; } }); }
  async function request(path, body) { return u.request(path, body ? { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) } : null, current); }
  function notice(e) { if (dead) return; var text = e.message || String(e); status.textContent = text; if (drawer) { var note = drawer.querySelector(".ac-feedback"); if (!note) { note = el("p", undefined, "ac-feedback"); note.setAttribute("role", "alert"); drawer.insertBefore(note, drawer.children[2] || null); } note.textContent = text; } }
  async function refresh() {
    var generation = ++dataGeneration;
    try { var latest = await request("/api/accounts"); if (dead || generation !== dataGeneration) return; data = latest; if (options.onProfiles) options.onProfiles(latest.profiles || []); fillSites(); status.textContent = latest.configured ? "选择站点，登录并核对身份后即可加入账号池。" : latest.message; render(); } catch (e) { if (generation === dataGeneration) notice(e); }
  }
  function fillSites() {
    var sites = Array.from(new Set((data.sites || []).concat((data.providers || []).map(function (d) { return d.site; }).concat((data.profiles || []).map(function (p) { return p.key.site; }))))).sort();
    siteFilter.replaceChildren(); var all = el("option", "全部站点"); all.value = ""; siteFilter.append(all);
    sites.forEach(function (site) { var o = el("option", site); o.value = site; siteFilter.append(o); });
    if (sites.indexOf(selectedSite) < 0) selectedSite = "";
    siteFilter.value = selectedSite;
  }
  function render() {
    content.replaceChildren(); if (!data) return;
    if (!data.providers.length) { content.append(el("p", "尚无站点接入账号登录。点击“新增账号”选择站点，查看接入状态。")); return; }
    var rows = (data.profiles || []).filter(function (p) { return !selectedSite || p.key.site === selectedSite; }).map(function (p) {
      var ops = el("div", undefined, "ac-ops"); ops.append(button(p.state === "ready" ? "重新登录" : "登录", function () { login(p); }), button("资料", function () { edit(p); }));
      if (p.state === "ready") ops.append(button("加入账号池", function () { join(p); }));
      return [p.key.site, p.label + " · " + p.key.id, p.state === "ready" ? "已核验" : "待登录", p.identity ? p.identity.display : "尚未识别", p.identity ? [p.identity.http ? "HTTP" : "", p.identity.browser ? "浏览器" : ""].filter(Boolean).join(" / ") : "—", u.date(p.verified_at), ops];
    });
    content.append(u.table(["站点", "账号", "接入状态", "真实身份", "已核验能力", "最近核验", "操作"], rows));
  }
  function close() { if (writing) return false; panelGeneration++; if (!rebuilding) { processVisible = false; stopRemote(true); } else { clearTimeout(remoteTimer); remoteImage = null; remotePages = null; remoteQueue = []; } u.dismissModal(drawer); drawer = null; return true; }
  function open(title, wide) { if (!close()) return null; drawer = u.modal(root, title, close, wide); drawer.append(el("h2", title)); return drawer; }
  function field(form, label, type, value) { var row = el("label", label), input = el(type === "textarea" ? "textarea" : "input"); if (type !== "textarea") input.type = type || "text"; input.value = value || ""; input.autocomplete = "off"; row.append(input); form.append(row); return input; }
  function loginFields(form, provider) {
    var select = el("select"), fields = el("div", undefined, "ac-fields"), values = {}, remember = el("input"); remember.type = "checkbox";
    select.setAttribute("aria-label", "登录方式");
    provider.methods.forEach(function (m) { var o = el("option", m.label); o.value = m.id; select.append(o); });
    var methodLabel = el("label", "登录方式"); methodLabel.append(select); methodLabel.hidden = provider.methods.length === 1;
    var rememberLabel = el("label", "保存登录信息，供下次登录使用", "ac-inline"); rememberLabel.prepend(remember);
    function build() { fields.replaceChildren(); values = {}; var m = provider.methods.find(function (v) { return v.id === select.value; }); (m.fields || []).forEach(function (f) { var input = field(fields, f.label, m.kind === "import" ? "textarea" : f.secret ? "password" : f.name === "email" ? "email" : "text"); input.required = !!f.required; input.dataset.loginField = f.name; values[f.name] = input; }); rememberLabel.hidden = !(m.fields || []).some(function (f) { return f.persist; }); remember.checked = false; }
    select.onchange = build; build(); form.append(methodLabel, fields, rememberLabel);
    return { method: function () { return select.value; }, remember: remember, values: function () { var v = {}; Object.keys(values).forEach(function (k) { v[k] = values[k].value; }); return v; }, clear: function () { Object.keys(values).forEach(function (k) { values[k].value = ""; }); } };
  }
  function edit(p) {
    if (!data) { notice(new Error("站点资料尚未加载，请稍后重试。")); return; }
    var box = open(p ? "账号资料" : "新增账号"); if (!box) return;
    var select = el("select"), label = el("label", "所属站点"), details = el("div");
    select.setAttribute("aria-label", "账号所属站点");
    var sites = Array.from(new Set((data.sites || []).concat((data.providers || []).map(function (d) { return d.site; })))).sort();
    var placeholder = el("option", sites.length ? "请选择站点" : "尚未注册站点"); placeholder.value = ""; select.append(placeholder);
    sites.forEach(function (site) { var provider = (data.providers || []).find(function (d) { return d.site === site; }); var o = el("option", provider ? provider.label + " · " + site : site); o.value = site; select.append(o); });
    select.value = p ? p.key.site : targetPool ? targetPool.key.site : selectedSite || (sites.length === 1 ? sites[0] : "");
    select.disabled = !!p || !!targetPool; label.append(select); box.append(label, details);
    function build() {
      details.replaceChildren();
      var site = select.value, provider = (data.providers || []).find(function (d) { return d.site === site; });
      if (!site) { details.append(el("p", "请先选择账号所属站点。", "rt-status")); return; }
      sessionStorage.setItem("papa-account-site", site);
      if (!data.configured || !provider || !(provider.methods || []).length) { details.append(el("p", "站点「" + site + "」的账号登录接入尚未施工，请先由该站点实现登录流程。", "rt-status")); return; }
      var form = el("form", undefined, "ac-form"), basic = el("div", undefined, "ac-fields"), name = field(basic, "账号备注（选填）", "text", p && p.label);
      var advanced = el("details"), advancedFields = el("div", undefined, "ac-fields"); advanced.append(el("summary", "高级：账号标识与身份约束"), advancedFields);
      var generated = "account_" + Array.from(crypto.getRandomValues(new Uint8Array(16))).map(function (b) { return b.toString(16).padStart(2, "0"); }).join("");
      var id = field(advancedFields, "内部账号 ID", "text", p ? p.key.id : generated), expected = field(advancedFields, "预期真实账号 ID（选填）", "text", p && p.expected_subject);
      id.required = true; id.disabled = !!p; expected.disabled = !!(p && p.state === "ready");
      advanced.append(el("p", "内部 ID 自动生成，用于关联任务。通常无需修改。", "ac-help"));
      var draft = p || null;
      form.append(basic);
      var inputs = p ? null : loginFields(form, provider);
      form.append(advanced);
      var submit = el("button", p ? "保存资料" : "登录并验证", "btn ac-primary"); submit.type = "submit";
      var actions = el("div", undefined, "ac-actions"); actions.append(submit, button("取消", close)); form.append(actions); details.append(form);
      form.onsubmit = async function (event) {
        event.preventDefault(); if (writing || !form.reportValidity()) return;
        var values = inputs ? inputs.values() : {}, display = name.value.trim() || values.email || values.username || values.account || id.value;
        setWriting(true);
        try {
          if (p || !draft) draft = await request("/api/accounts", { action: "draft", revision: p ? p.revision : 0, profile: { key: { site: site, id: id.value }, label: display, expected_subject: expected.value } });
          if (dead) return;
          if (p) { setWriting(false); close(); await refresh(); if (options.embedded) manage(targetPool); return; }
          process = await request("/api/accounts", { action: "start", key: draft.key, revision: draft.revision, method: inputs.method(), input: values, remember: inputs.remember.checked });
          if (dead) return;
          inputs.clear(); setWriting(false); sessionStorage.setItem("papa-account-login", process.id); showProcess(true); schedule();
        } catch (e) { setWriting(false); if (draft) { id.disabled = true; name.disabled = true; expected.disabled = true; select.disabled = true; } notice(e); }
      };
    }
    select.onchange = build; build();
  }
  function login(p) {
    var provider = data.providers.find(function (v) { return v.site === p.key.site; });
    if (!provider || !(provider.methods || []).length) { notice(new Error("站点「" + p.key.site + "」的账号登录接入尚未施工。")); return; }
    var box = open("登录 · " + (p.label === p.key.id ? p.identity ? p.identity.display : p.key.site : p.label)); if (!box) return;
    var form = el("form", undefined, "ac-form"), inputs = loginFields(form, provider), submit = el("button", "开始登录", "btn ac-primary"); submit.type = "submit";
    var actions = el("div", undefined, "ac-actions"); actions.append(submit, button("取消", close)); form.append(el("p", "登录失败时保留原来可用的登录态。", "ac-help"), actions); box.append(form);
    form.onsubmit = async function (event) {
      event.preventDefault(); if (writing || !form.reportValidity()) return; setWriting(true);
      try { process = await request("/api/accounts", { action: "start", key: p.key, revision: p.revision, method: inputs.method(), input: inputs.values(), remember: inputs.remember.checked }); if (dead) return; inputs.clear(); setWriting(false); sessionStorage.setItem("papa-account-login", process.id); showProcess(true); schedule(); }
      catch (e) { setWriting(false); notice(e); }
    };
  }
  async function retryLogin() {
    var key = process.key, generation = panelGeneration; await refresh();
    if (dead || generation !== panelGeneration) return;
    var profile = data.profiles.find(function (p) { return p.key.site === key.site && p.key.id === key.id; });
    if (profile) login(profile); else notice(new Error("账号资料已变化，请关闭后刷新账号列表。"));
  }
  var names = { running: "正在处理", waiting_input: "等待输入", waiting_scan: "等待扫码", remote: "等待页面操作", confirm_identity: "请核对真实身份", succeeded: "账号已保存并生效", failed: "登录未完成", expired: "登录已过期", cancelled: "登录已取消" };
  var failures = { identity_mismatch: "真实账号与预期绑定不一致，候选已拒绝。", identity_verification_failed: "未能确认账号身份或权限。", candidate_publish_failed: "凭据发布未完成；请核对资料版本及原账号状态。", remote_disconnected: "远程控制连接已断开并回收。", login_expired: "登录过程已过期，请重新发起。", login_budget_wait: "账号登录间隔尚未结束，请稍后再试。", health_restore_pending: "凭据已更新，恢复状态待保存。请到账号池核对；人工停用状态保持不变。" };
  function terminal() { return process && ["succeeded", "failed", "expired", "cancelled"].includes(process.state); }
  function showProcess(force) {
    if (!process || dead || (!force && !processVisible)) return; if (!force && drawer && drawer.dataset.process === process.id && drawer.dataset.revision === String(process.revision)) return;
    rebuilding = true; var box = open("登录进度", process.state === "remote"); rebuilding = false; if (!box) return; processVisible = true; box.dataset.process = process.id; box.dataset.revision = String(process.revision);
    box.append(el("p", names[process.state] || process.state, "ac-flow"), el("p", "站点 " + process.key.site + " · 有效至 " + u.date(process.expires_at), "rt-muted"));
    if (process.failure) box.append(el("p", failures[process.failure] || "站点登录或检查未完成，请重新核对后发起。", "rt-status"));
    if (process.image_data_url) { var qr = el("img"); qr.src = process.image_data_url; qr.alt = "站点登录二维码"; qr.style.maxWidth = "300px"; box.append(qr); }
    if (process.state === "confirm_identity") { var i = process.identity; box.append(el("div", "识别到的账号：" + i.display + "\n真实 ID：" + i.subject + "\n权限：" + (i.scopes || []).join("、") + "\n能力：" + [i.http ? "HTTP" : "", i.browser ? "浏览器" : ""].filter(Boolean).join(" / "), "ac-identity"), button(targetPool ? "确认身份并加入此池" : "身份正确，保存并启用", function () { act("confirm", {}); })); }
    if (["waiting_input", "waiting_scan", "remote"].includes(process.state)) {
      var form = el("div", undefined, "ac-fields"), inputs = {}; (process.fields || []).forEach(function (f) { inputs[f.name] = field(form, f.label, f.secret ? "password" : "text"); }); box.append(form);
      if (process.state === "remote") remotePanel(box);
      box.append(button(process.state === "waiting_scan" ? "检查扫码结果" : process.state === "remote" ? "已完成页面登录，检查身份" : "提交本次输入", function () { var v = {}; Object.keys(inputs).forEach(function (k) { v[k] = inputs[k].value; }); act("continue", v); }));
    }
    if (!terminal()) box.append(button("取消登录", function () { act("cancel", {}); })); else { sessionStorage.removeItem("papa-account-login"); sessionStorage.removeItem("papa-account-control"); if (["failed", "expired", "cancelled"].includes(process.state)) box.append(button("重新登录", retryLogin), button("管理账号", function () { manage(targetPool); })); if (process.state === "succeeded") { box.append(el("p", "登录已验证。加入池后即可用于采集。", "ac-help"), button(targetPool ? "加入当前池" : "加入账号池", function () { join({ key: process.key, label: process.key.id }); }), button("管理账号", function () { manage(targetPool); })); if (options.onChange) options.onChange(); } refresh(); }
  }
  function finishPendingJoin() {
    if (!pendingJoin || !process || process.id !== pendingJoin.id || process.state !== "succeeded") return;
    var intent = pendingJoin; pendingJoin = null;
    if (targetPool && targetPool.key.site === intent.site && targetPool.key.name === intent.name) changeMember(process.key, true);
  }
  async function act(action, input) {
    if (writing || !process) return;
    if (action === "confirm" && targetPool) pendingJoin = { id: process.id, site: targetPool.key.site, name: targetPool.key.name };
    setWriting(true);
    try { var next = await request("/api/accounts", { action: action, id: process.id, revision: process.revision, input: input }); if (dead) return; setWriting(false); process = next; showProcess(true); schedule(); finishPendingJoin(); }
    catch (e) { pendingJoin = null; setWriting(false); notice(e); await poll(); }
  }
  function schedule() { clearTimeout(timer); if (!dead && process && !terminal()) timer = setTimeout(poll, 1200); }
  async function poll() { if (!process || dead) return; var id = process.id; try { var next = await request("/api/accounts?process=" + encodeURIComponent(id)); if (dead || !process || process.id !== id) return; if (next.revision >= (process.revision || 0)) process = next; if (!writing) { showProcess(false); finishPendingJoin(); } } catch (e) { if (!process || process.id !== id) return; notice(e); if ([403, 410].includes(e.status)) { sessionStorage.removeItem("papa-account-login"); process = null; stopRemote(false); } } schedule(); }
  async function join(p) {
    var generation = panelGeneration;
    if (targetPool) { await changeMember(p.key, true); return; }
    try { var d = await request("/api/account-pools"); if (dead || generation !== panelGeneration) return; var pools = d.pools.filter(function (v) { return v.key.site === p.key.site; }), box = open("加入账号池 · " + p.label); if (!box) return; var select = el("select"); pools.forEach(function (v, i) { var o = el("option", v.key.name + ((v.members || []).includes(p.key.id) ? "（已加入）" : "")); o.value = String(i); select.append(o); }); box.append(el("p", "只列出同站账号池。请按实际权限分池；加入成员不会抢占当前健康账号。"), select); if (!pools.length) { box.append(el("p", "本站还没有账号池。创建后会将此账号加入。")); if (options.createPool) box.append(button("创建账号池", function () { if (close()) options.createPool(p.key.site, p.key.id); })); return; }
      box.append(button("加入所选池", async function () { if (writing) return; var v = pools[Number(select.value)]; if ((v.members || []).includes(p.key.id)) { status.textContent = "账号已经在池中。"; return; } setWriting(true); var cfg = {}; ["key", "members", "transport", "disabled", "verify_attempts", "switch_limit", "cooldown", "standby_target", "standby_ttl"].forEach(function (k) { if (v[k] !== undefined) cfg[k] = v[k]; }); cfg.members = (v.members || []).concat(p.key.id); try { await request("/api/account-pools", { action: "save", revision: v.revision, config: cfg }); setWriting(false); close(); status.textContent = "已加入账号池。"; } catch (e) { setWriting(false); notice(e); } }));
    } catch (e) { notice(e); }
  }
  async function changeMember(key, adding) {
    if (writing || !targetPool) return; setWriting(true);
    try {
      var latest = await request("/api/account-pools"); if (dead) return; var pool = latest.pools.find(function (p) { return p.key.site === targetPool.key.site && p.key.name === targetPool.key.name; });
      if (!pool) throw new Error("此账号池已不存在，请关闭弹窗刷新列表。"); pool.members = pool.members || [];
      if (key.site !== pool.key.site) throw new Error("只能加入同站账号。");
      var cfg = {}; ["key", "members", "transport", "disabled", "verify_attempts", "switch_limit", "cooldown", "standby_target", "standby_ttl"].forEach(function (k) { if (pool[k] !== undefined) cfg[k] = pool[k]; });
      cfg.members = adding ? pool.members.slice() : pool.members.filter(function (id) { return id !== key.id; }); if (adding && !cfg.members.includes(key.id)) cfg.members.push(key.id);
      await request("/api/account-pools", { action: "save", revision: pool.revision, config: cfg });
      if (dead) return; targetPool = Object.assign({}, pool, cfg); setWriting(false); if (options.onChange) options.onChange(); await manage(targetPool);
    } catch (e) { setWriting(false); notice(e); }
  }
  async function manage(pool, site) {
    if (writing) return; targetPool = pool || null; if (pool) selectedSite = pool.key.site; else if (site !== undefined) selectedSite = site;
    var box = open(pool ? "管理账号 · " + pool.key.name : "站点账号", true); if (!box) return;
    var owned = panelGeneration; box.append(el("p", "正在读取账号…", "rt-status")); await refresh();
    if (dead || drawer !== box || owned !== panelGeneration || !data) return;
    if (pool) {
      try { var result = await request("/api/account-pools"); if (dead || drawer !== box || owned !== panelGeneration) return; var latest = result.pools.find(function (p) { return p.key.site === pool.key.site && p.key.name === pool.key.name; }); if (!latest) throw new Error("此池已删除，请关闭弹窗后刷新列表。"); pool = latest; pool.members = pool.members || []; targetPool = pool; }
      catch (e) { if (!dead && drawer === box) notice(e); return; }
    }
    box.querySelector(".rt-status").remove();
    var head = el("div", undefined, "ac-manager-head"); head.append(el("p", pool ? "站点 " + pool.key.site + " · 添加、登录或调整此池成员" : "查看账号资料并选择要加入的池", "ac-help"), button("新增账号", function () { edit(); }), button("刷新账号", function () { manage(targetPool); })); box.append(head);
    var profiles = (data.profiles || []).filter(function (p) { return !selectedSite || p.key.site === selectedSite; }).slice();
    (data.runtime || []).forEach(function (r) { if ((!selectedSite || r.key.site === selectedSite) && !profiles.some(function (p) { return p.key.site === r.key.site && p.key.id === r.key.id; })) profiles.push({ key: r.key, label: r.key.id, state: "external", external: true }); });
    var rows = profiles.map(function (p) {
      var member = pool && pool.members.includes(p.key.id), identity = el("div", p.label && p.label !== p.key.id ? p.label : p.identity ? p.identity.display : p.key.id), ops = el("div", undefined, "ap-ops");
      if (!p.external) ops.append(button(p.state === "ready" ? "重新登录" : "登录", function () { login(p); }), button("资料", function () { edit(p); }));
      if (member) ops.append(button("移出此池", function () { changeMember(p.key, false); }));
      else if (p.state === "ready" || p.external) ops.append(button(pool ? "加入此池" : "加入账号池", function () { join(p); }));
      var row = [identity, member ? "已加入" : "未加入", p.state === "ready" ? "已核验" : p.external ? "业务注册" : "待登录", p.identity ? p.identity.display : "—", ops]; if (!pool) row.unshift(p.key.site); return row;
    });
    var headers = ["账号", "池成员", "登录状态", "真实身份", "操作"]; if (!pool) headers.unshift("站点"); box.append(u.table(headers, rows));
    if (!rows.length) box.append(el("p", "本站还没有账号。点击“新增账号”完成登录。", "ap-empty"));
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
  var dispose = function () { stopRemote(true); dead = true; clearTimeout(timer); clearTimeout(remoteTimer); if (removeKeyboard) removeKeyboard(); u.dismissModal(drawer); };
  dispose.manage = manage;
  dispose.add = async function (site) { targetPool = null; selectedSite = site || ""; await refresh(); if (!dead) edit(); };
  dispose.close = close;
  return dispose;
}
window.PapaAccounts = { mount: mountAccounts };
Papa.page("accounts", function (root) { return window.PapaAccountPools.mount(root); });
})();
