Papa.page("account-pools", function (root) {
  "use strict";
  var u = window.PapaRuntime,
    el = u.el,
    button = u.button,
    dead = false,
    generation = 0,
    data = null,
    drawer = null,
    dirty = false,
    writing = false;
  var bar = el("div", undefined, "rt-toolbar"),
    status = el("p", "正在读取账号池…", "rt-status"),
    content = el("div"),
    search = el("input");
  status.setAttribute("role", "status");
  search.type = "search";
  search.placeholder = "站点或池名";
  search.setAttribute("aria-label", "筛选账号池");
  search.oninput = render;
  bar.append(
    el("h2", "账号池"),
    search,
    button("创建池", function () {
      edit(null);
    }),
    button("添加与管理账号", function () { openPage("accounts", "账号管理"); }),
    button("刷新", refresh),
  );
  root.append(bar, status, content);
  function current() {
    return !dead;
  }
  async function refresh(force) {
    if (writing && !force) return;
    var n = ++generation;
    try {
      var d = await u.request("/api/account-pools", null, function () {
        return !dead && n === generation;
      });
      if (dead || n !== generation) return;
      data = d;
      status.textContent = d.configured
        ? "疑似失效先核验；额度不足等待，确认失效才切换备用。"
        : "业务尚未注册账号管理器，请先通过公开 API 注册账号。";
      render();
    } catch (e) {
      if (!dead && n === generation) status.textContent = e.message;
    }
  }
  function close(force) {
    if (writing && !force) return false;
    if (dirty && !force && !window.confirm("放弃未保存的修改？")) return false;
    if (drawer) drawer.remove();
    drawer = null;
    dirty = false;
    return true;
  }
  function render() {
    if (!data) return;
    content.replaceChildren();
    var rows = (data.pools || [])
      .filter(function (p) {
        return (p.key.site + " " + p.key.name).indexOf(search.value) >= 0;
      })
      .map(function (p) {
        var ops = el("div");
        ops.append(
          button("查看", function () {
            detail(p);
          }),
          button("设置", function () {
            edit(p);
          }),
        );
        return [
          p.key.site,
          p.key.name,
          p.current_id || "等待选择",
          p.available + " / " + p.members.length,
          ["needs-verification", "invalid", "disabled", "cooling"]
            .map(function (state) {
              return p.accounts.filter(function (m) {
                return m.state === state;
              }).length;
            })
            .join(" / "),
          p.disabled ? "已停用" : p.wait_reason || "可用",
          p.waiting,
          (p.history || []).length
            ? u.date(p.history[p.history.length - 1].at) +
              " · " +
              p.history[p.history.length - 1].reason
            : "—",
          ops,
        ];
      });
    content.appendChild(
      u.table(
        [
          "站点",
          "池名",
          "当前账号",
          "可用 / 总数",
          "待核验 / 失效 / 停用 / 冷却",
          "状态",
          "等待任务",
          "最近切换原因",
          "操作",
        ],
        rows,
      ),
    );
    if (!rows.length)
      content.appendChild(
        el("p", "暂无账号池。账号凭据仍由业务注册与凭据存储管理。"),
      );
  }
  async function act(body) {
    if (writing) return;
    writing = true;
    var frozen = drawer ? Array.from(drawer.querySelectorAll("input:not(:disabled),select:not(:disabled),button:not(:disabled)")) : [];
    frozen.forEach(function (x) { x.disabled = true; });
    generation++;
    try {
      var result = await u.request(
        "/api/account-pools",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        },
        current,
      );
      if (dead) return;
      data = result;
      close(true);
      render();
      status.textContent =
        body.action === "verify" || body.action === "check"
          ? "已提交" + (body.action === "check" ? "原状态检查" : "恢复登录") + "，刷新详情可查看结果。"
          : "操作已完成。";
    } catch (e) {
      if (!dead) {
        status.textContent = e.message + "；未自动重试。";
        if (drawer)
          drawer.appendChild(
            el("p", e.message + "；草稿已保留，请核对最新状态。", "rt-status"),
          );
        await refresh(true);
        if (drawer && e.status === 409 && body.action === "save") {
          var latest = e.data && e.data.deleted;
          if (!latest && e.data && e.data.latest)
            latest = (e.data.latest.pools || []).find(function (p) {
              return (
                p.key.site === body.config.key.site &&
                p.key.name === body.config.key.name
              );
            });
          if (latest) {
            drawer.appendChild(
              el(
                "pre",
                JSON.stringify(
                  {
                    revision: latest.revision,
                    deleted: !!latest.deleted,
                    members: latest.members,
                    transport: latest.transport,
                    disabled: latest.disabled,
                    verify_attempts: latest.verify_attempts,
                    switch_limit: latest.switch_limit,
                    cooldown: latest.cooldown,
                    standby_target: latest.standby_target,
                    standby_ttl: latest.standby_ttl,
                  },
                  null,
                  2,
                ),
                "rt-preview",
              ),
            );
            drawer.appendChild(
              button(
                latest.deleted
                  ? "按此删除记录版本重新创建"
                  : "已核对，按最新版本保存当前设置",
                function () {
                  body.revision = latest.revision;
                  act(body);
                },
              ),
            );
          }
        }
      }
    } finally {
      writing = false;
      frozen.forEach(function (x) { if (x.isConnected) x.disabled = false; });
    }
  }
  function detail(p) {
    if (!close()) return;
    drawer = el("aside", undefined, "rt-drawer");
    drawer.setAttribute("role", "dialog");
    drawer.setAttribute("aria-label", "账号池详情");
    drawer.append(
      button("关闭", function () {
        close();
      }),
      el("h2", p.key.site + " / " + p.key.name),
      el(
        "p",
        "当前账号：" +
          (p.current_id || "待选择") +
          " · 选择版本 " +
          p.selection_version,
      ),
      button("刷新此池", async function () {
        var pane = drawer;
        await refresh();
        if (dead || drawer !== pane) return;
        var latest = data.pools.find(function (x) { return x.key.site === p.key.site && x.key.name === p.key.name; });
        if (latest) detail(latest);
      }),
      button("池关联任务", function () { compensation(p.key); }),
    );
    if (p.persistence_error)
      drawer.appendChild(
        el("p", "状态尚未持久化：" + p.persistence_error, "rt-status"),
      );
    drawer.appendChild(
      u.table(
        [
          "成员",
          "状态",
          "HTTP / 浏览器",
          "使用 / 上限",
          "管理版本",
          "原因",
          "核验进度 / 下次检查",
          "使用预算",
          "冷却至",
          "浏览器身份",
          "操作",
        ],
        p.accounts.map(function (m) {
          var identity = (data.identity_bindings || []).find(function (b) { return b.subject.kind === 'account' && b.subject.site === m.key.site && b.subject.key === m.key.id; });
          var recent = (data.identity_reports || []).find(function (r) { return r.subject.kind === 'account' && r.subject.site === m.key.site && r.subject.key === m.key.id; });
          var identityCell = el('div', data.identity_error || (identity && identity.active ? identity.active.id + ' · v' + identity.active.version : '未绑定'));
          if (identity && identity.pending) identityCell.append(el('small', ' 待生效：' + identity.pending.id + ' · v' + identity.pending.version));
          if (recent) identityCell.append(el('p', (recent.ok ? '本地检查一致' : '本地检查待核对') + ' · ' + u.date(recent.at), 'rt-muted'));
          identityCell.append(button('档案与检查', function () { window.PapaIdentityTarget = {site:m.key.site, kind:'account', key:m.key.id}; openPage('browser-diagnostics', '浏览器诊断'); }));
          var ops = el("div");
          ops.append(
            button(
              m.management.disabled ? "恢复验证资格" : "停用",
              function () {
                act({
                  action: "disable",
                  account: m.key,
                  disabled: !m.management.disabled,
                  revision: m.management.version,
                });
              },
            ),
            button("检查原登录态", function () {
              act({ action: "check", account: m.key, revision: m.management.version });
            }),
            button("恢复登录", function () {
              act({
                action: "verify",
                account: m.key,
                revision: m.management.version,
              });
            }),
            button("健康与使用策略", function () { policy(m); }),
            button("关联任务", function () { compensation(p.key, m.key.id); }),
          );
          return [
            m.key.id,
            stateLabel(m.state) + (m.persisted ? "" : "（未保存）"),
            (m.http_ready ? "就绪" : "未验证") +
              " / " +
              (m.browser_ready ? "就绪" : "未验证"),
            m.in_use + " / " + (m.max_concurrent || "不限"),
            m.management.version,
            m.management.reason || m.failure || "—",
            (m.verifying ? "正在" : "") + (m.management.verification_mode === "check-existing" ? "检查原状态" : m.management.verification_mode === "recover-login" ? "恢复登录" : "—") +
              " · 无效确认 " + m.management.invalid_checks + " · 未知 " + m.management.unknown_checks +
              " · 下次 " + u.date(m.management.next_verify_at) + " · 上次结果 " + (m.management.last_verification || "—"),
            (m.management.policy && (m.management.policy.track_usage || m.management.policy.daily_limit || m.management.policy.operation_interval)) ? ((m.management.usage || {}).operations || 0) + " / " + (m.management.policy.daily_limit || "不限") + " 次操作 · " + ((m.management.usage || {}).day || "—") : "未启用操作计量",
            u.date(m.management.cooldown_until),
            identityCell,
            ops,
          ];
        }),
      ),
    );
    drawer.append(
      el("h3", "健康事件（最近记录）"),
      u.table(["时间", "账号", "事件", "原因", "任务"], (data.events || []).filter(function (e) {
        return e.key.site === p.key.site && p.members.indexOf(e.key.id) >= 0 && (!e.pool || !e.pool.name || e.pool.name === p.key.name);
      }).slice().reverse().map(function (e) { return [u.date(e.at), e.key.id, stateLabel(e.kind), e.reason, e.task_id || "—"]; })),
      el("h3", "切换记录"),
      u.table(
        ["时间", "原账号 → 新账号", "原因", "触发任务", "选择版本"],
        (p.history || [])
          .slice()
          .reverse()
          .map(function (h) {
            return [
              u.date(h.at),
              (h.from || "—") + " → " + (h.to || "—"),
              h.reason,
              h.task_id || "—",
              h.selection_version,
            ];
          }),
      ),
      el(
        "p",
        "切换与任务补偿独立进行。只有满足持久补偿条件的原任务才能重新投递；账号关联保留最近 8 次执行记录。",
        "rt-muted",
      ),
    );
    root.appendChild(drawer);
  }
  function edit(p) {
    if (!data || !data.configured || !close()) return;
    var cfg = p
      ? {
          key: u.clone(p.key),
          members: p.members.slice(),
          transport: p.transport,
          disabled: p.disabled,
          verify_attempts: p.verify_attempts,
          switch_limit: p.switch_limit,
          cooldown: p.cooldown,
          standby_target: p.standby_target,
          standby_ttl: p.standby_ttl,
        }
      : {
          key: { site: "", name: "" },
          members: [],
          transport: "http",
          disabled: false,
          verify_attempts: 1,
          switch_limit: 16,
          cooldown: 5e9,
          standby_target: 0,
          standby_ttl: 900e9,
        };
    drawer = el("aside", undefined, "rt-drawer");
    drawer.setAttribute("role", "dialog");
    drawer.setAttribute("aria-label", p ? "账号池设置" : "创建账号池");
    drawer.appendChild(el("h2", p ? "账号池设置" : "创建账号池"));
    var form = el("form");
    form.onsubmit = function (e) {
      e.preventDefault();
    };
    function input(label, value, type, change) {
      var row = el("label", undefined, "rt-field"),
        x = el("input");
      x.type = type || "text";
      if (type === "checkbox") x.checked = value;
      else x.value = value;
      x.oninput = function () {
        dirty = true;
        change(
          type === "checkbox"
            ? x.checked
            : type === "number"
              ? Number(x.value)
              : x.value,
        );
      };
      row.append(el("span", label), x);
      form.appendChild(row);
      return x;
    }
    var site = input("站点", cfg.key.site, "text", function (v) {
      cfg.key.site = v;
      cfg.members = [];
      members();
    });
    site.required = true;
    site.disabled = !!p;
    var name = input("池名", cfg.key.name, "text", function (v) {
      cfg.key.name = v;
    });
    name.required = true;
    name.disabled = !!p;
    var row = el("label", undefined, "rt-field"),
      transport = el("select");
    [
      ["http", "HTTP"],
      ["browser", "浏览器"],
      ["both", "两者均需验证"],
    ].forEach(function (pair) {
      var o = el("option", pair[1]);
      o.value = pair[0];
      transport.appendChild(o);
    });
    transport.value = cfg.transport;
    transport.onchange = function () {
      dirty = true;
      cfg.transport = transport.value;
    };
    row.append(el("span", "使用方式"), transport);
    form.appendChild(row);
    input("停用整个池", cfg.disabled, "checkbox", function (v) {
      cfg.disabled = v;
    });
    var tries = input(
      "每成员验证次数",
      cfg.verify_attempts,
      "number",
      function (v) {
        cfg.verify_attempts = v;
      },
    );
    tries.min = 1;
    tries.max = 10;
    var limit = input(
      "每轮替补检查上限",
      cfg.switch_limit,
      "number",
      function (v) {
        cfg.switch_limit = v;
      },
    );
    limit.min = 1;
    limit.max = 1024;
    var cooldown = input(
      "补偿冷却（秒）",
      cfg.cooldown / 1e9,
      "number",
      function (v) {
        cfg.cooldown = v * 1e9;
      },
    );
    cooldown.min = 0.001;
    cooldown.step = "any";
    var standby = input("预验证备用账号数（0 关闭）", cfg.standby_target || 0, "number", function (v) { cfg.standby_target = v; });
    standby.min = 0;
    var ttl = input("备用验证有效期（秒）", (cfg.standby_ttl || 900e9) / 1e9, "number", function (v) { cfg.standby_ttl = v * 1e9; });
    ttl.min = 0.001;
    ttl.step = "any";
    var memberBox = el("div");
    form.append(
      el("h3", "备用顺序"),
      el(
        "p",
        "正常完成任务不会轮换。当前账号失效后，按此顺序选择已验证成员。",
        "rt-muted",
      ),
      memberBox,
    );
    function members() {
      memberBox.replaceChildren();
      cfg.members.forEach(function (id, i) {
        var r = el("div", undefined, "rt-members");
        r.append(
          el("span", i + 1 + ". " + id),
          button("上移", function () {
            if (i > 0) {
              cfg.members.splice(i, 1);
              cfg.members.splice(i - 1, 0, id);
              dirty = true;
              members();
            }
          }),
          button("移出", function () {
            cfg.members.splice(i, 1);
            dirty = true;
            members();
          }),
        );
        memberBox.appendChild(r);
      });
      var select = el("select");
      (data.accounts || [])
        .filter(function (a) {
          return (
            a.key.site === cfg.key.site && cfg.members.indexOf(a.key.id) < 0
          );
        })
        .forEach(function (a) {
          var o = el("option", a.key.id);
          o.value = a.key.id;
          select.appendChild(o);
        });
      memberBox.append(
        select,
        button("加入已注册账号", function () {
          if (select.value) {
            cfg.members.push(select.value);
            dirty = true;
            members();
          }
        }),
      );
    }
    members();
    drawer.append(
      form,
      button("保存", function () {
        if (form.reportValidity())
          act({ action: "save", revision: p ? p.revision : 0, config: cfg });
      }),
      button("取消", function () {
        close();
      }),
    );
    if (p)
      drawer.appendChild(
        button("删除已停用池", function () {
          if (
            window.confirm("删除池配置？有策略或可重投任务引用时会拒绝删除。")
          )
            act({ action: "delete", key: p.key, revision: p.revision });
        }),
      );
    root.appendChild(drawer);
    site.focus();
  }
  function stateLabel(value) {
    return ({"needs-verification":"待核验", "available":"可用", "invalid":"已隔离", "disabled":"已停用", "cooling":"冷却", "verifying":"核验中", "pending":"待验证", "proxy-unavailable":"代理不可用", "daily-budget-exhausted":"日预算已用尽", "operation-interval":"使用间隔", "outside-work-hours":"非工作时段", "manual":"等待人工", "permanent":"永久终止", "retry_after":"等待时间", "account_ready":"等待账号就绪", "recovered":"已恢复", "verification-unknown":"核验结果未知", "verification-invalid":"无效核验结果", "verification-overdue":"核验等待过久", "pool-exhausted":"账号池耗尽", "standby-shortage":"备用不足", "manual-verification":"待人工验证", "manual intervention required":"需人工解除", "permanently terminated":"永久终止", "retry-after deadline has not elapsed":"未到最早补偿时间", "account pools are not configured":"尚未配置账号池", "account pool is not ready":"账号池未就绪", "task stage is not registered":"任务阶段未注册", "account-ready compensation requires task pool ownership":"等待账号就绪的任务缺少账号池归属"})[value] || value || "默认补偿";
  }
  function policy(member) {
    if (!close()) return;
    var value = Object.assign({}, data.default_policy || {}, member.management.policy || {}), form = el("form");
    drawer = el("aside", undefined, "rt-drawer");
    drawer.setAttribute("role", "dialog");
    drawer.setAttribute("aria-label", "账号健康与使用策略");
    drawer.append(el("h2", member.key.site + " / " + member.key.id), el("p", "策略按账号跨池共享。计量单位为 HTTP 交换和浏览器操作，不是页面内所有网络请求。额度不足时等待，不会换号。", "rt-muted"));
    form.onsubmit = function (e) { e.preventDefault(); };
    function field(label, key, unit, min, max) {
      var input = el("input"), row = el("label", undefined, "rt-field");
      input.type = unit === "text" ? "text" : unit === "clock" ? "time" : "number";
      input.value = unit === "seconds" ? value[key] / 1e9 : unit === "clock" ? String(Math.floor(value[key] / 60)).padStart(2,"0") + ":" + String(value[key] % 60).padStart(2,"0") : value[key];
      if (input.type === "number") { input.min = min === undefined ? 0 : min; input.step = unit === "seconds" ? "any" : "1"; if (max !== undefined) input.max = max; }
      input.required = true;
      input.oninput = function () { value[key] = unit === "text" ? input.value : unit === "clock" ? Number(input.value.split(":")[0]) * 60 + Number(input.value.split(":")[1]) : Number(input.value) * (unit === "seconds" ? 1e9 : 1); dirty = true; };
      row.append(el("span", label), input); form.appendChild(row);
    }
    form.appendChild(el("h3", "疑似失效与独立核验"));
    field("首次核验缓冲（秒）", "buffer", "seconds", 0.001);
    field("确认无效次数", "invalid_confirmations", "number", 1, 10);
    field("无效复核间隔（秒）", "check_interval", "seconds", 0.001);
    field("单次核验超时（秒）", "verify_timeout", "seconds", 0.001, 600);
    field("未知结果首次退避（秒）", "unknown_backoff", "seconds", 0.001);
    field("最大退避（秒）", "max_backoff", "seconds", 0.001);
    field("持续未知告警等待（秒）", "unknown_alert_after", "seconds", 0.001);
    form.appendChild(el("h3", "使用与登录节奏"));
    var track = el("input"), trackRow = el("label", undefined, "rt-field");
    track.type = "checkbox"; track.checked = !!value.track_usage;
    track.onchange = function () { value.track_usage = track.checked; dirty = true; };
    trackRow.append(el("span", "未设额度时仍持久记录每次操作"), track); form.appendChild(trackRow);
    form.appendChild(el("p", "默认不逐操作写库；启用计量、日上限或操作间隔后，每次放行先持久预留，数据库写入成本随操作量增加。", "rt-muted"));
    field("自动恢复登录间隔（秒，0 继承程序配置）", "recovery_interval", "seconds");
    form.appendChild(el("p", "程序默认关闭自动恢复；若业务设置全局恢复间隔，0 表示沿用它。自动恢复只处理已隔离且无需人工挑战的账号，恢复后不抢回当前选择。", "rt-muted"));
    field("操作间隔（秒，0 不限）", "operation_interval", "seconds");
    field("每日操作上限（0 不限）", "daily_limit", "number");
    field("时区（如 Asia/Shanghai）", "timezone", "text");
    field("工作开始时间", "work_start_minute", "clock");
    field("工作结束时间", "work_end_minute", "clock");
    form.appendChild(el("p", "起止相同表示全天；开始晚于结束表示跨午夜。", "rt-muted"));
    field("登录最短间隔（秒，0 不限）", "login_interval", "seconds");
    drawer.append(form, button("保存策略", function () { if (form.reportValidity()) act({action:"policy", account:member.key, revision:member.management.version, policy:value}); }), button("取消", function () { close(); }));
    root.appendChild(drawer);
  }
  async function compensation(key, accountID) {
    if (!close()) return;
    var pane = el("aside", undefined, "rt-drawer"), note = el("p", "正在预览…", "rt-status"), rows = el("div"), results = el("div"), selected = [], previewGeneration = 0;
    drawer = pane;
    pane.setAttribute("role", "dialog"); pane.setAttribute("aria-label", "关联任务与补偿预览");
    var apply = button("补偿所选任务", function () { submit({action:"retry", requests:selected.slice()}); });
    apply.disabled = true;
    pane.append(el("h2", "关联任务 · " + key.site + " / " + key.name + (accountID ? " / " + accountID : "")), el("p", "最多显示最近 200 条失败任务；账号匹配最近 8 次执行。预览不重投，实际提交再次检查条件与版本。", "rt-muted"), note, button("重新预览", preview), rows, apply, results, button("关闭", function () { close(); }));
    root.appendChild(pane);
    function live() { return !dead && drawer === pane; }
    async function preview() {
      if (!live() || writing) return;
      if (dirty && !window.confirm("清空当前选择并重新预览？")) return;
      var n = ++previewGeneration;
      try {
        var query = new URLSearchParams({site:key.site, pool:key.name, limit:"200"}); if (accountID) query.set("account", accountID);
        var result = await u.request("/api/account-compensation?" + query.toString(), null, function () { return live() && n === previewGeneration; });
        if (!live() || n !== previewGeneration) return;
        dirty = false; selected = []; apply.disabled = true;
        note.textContent = "本页可补偿 " + result.eligible + " · 被阻止 " + result.blocked + (result.truncated ? " · 还有更多匹配任务，请到任务管理按站点筛选" : "");
        rows.replaceChildren(u.table(["选择", "任务 / 阶段", "补偿条件", "原因 / 阻止原因", "最早时间", "最近账号尝试", "操作"], result.items.map(function (item) {
          var check = el("input"), ops = el("div"); check.type = "checkbox"; check.disabled = !item.eligible; check.setAttribute("aria-label", "选择任务 " + item.id);
          check.onchange = function () { selected = selected.filter(function (x) { return x.id !== item.id; }); if (check.checked) selected.push({id:item.id,version:item.version}); dirty = !!selected.length; apply.disabled = !dirty || writing; };
          if (item.mode === "manual") ops.appendChild(button("解除人工等待", function () { if (window.confirm("只解除任务 " + item.id + " 的人工等待条件，不会重投；随后需重新预览。")) submit({action:"release",id:item.id,version:item.version}); }));
          return [check, item.id + " / " + item.stage, stateLabel(item.mode), (item.reason || "—") + (item.blocked_reason ? "\n" + stateLabel(item.blocked_reason) : ""), u.date(item.next_compensation_at), (item.account_attempts || []).map(function (a) { return a.account_id + " · " + u.date(a.at); }).join("\n") || "无已记录账号", ops];
        })));
        if (!result.items.length) rows.appendChild(el("p", "没有匹配的失败任务。"));
      } catch (e) { if (live() && n === previewGeneration) note.textContent = e.message; }
    }
    async function submit(body) {
      if (!live() || writing) return;
      writing = true; apply.disabled = true; previewGeneration++;
      // A pending mutation freezes its snapshot; stale checkboxes cannot enable a second write.
      rows.querySelectorAll("input,button").forEach(function (x) { x.disabled = true; });
      try {
        var result = await u.request("/api/account-compensation", {method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(body)}, live);
        if (!live()) return;
        dirty = false; selected = [];
        results.replaceChildren();
        if (result.results) results.appendChild(u.table(["任务", "结果"], result.results.map(function (r) { return [r.id, r.submitted ? "已提交" : r.error || "未提交"]; })));
        note.textContent = result.message || "提交结果如下；重新预览后才能再次操作。";
      } catch (e) {
        if (live()) {
          note.textContent = e.message + "；未自动重试，请重新预览核对。";
          if (e.data && e.data.results) results.replaceChildren(u.table(["任务", "已知结果"], e.data.results.map(function (r) { return [r.id, r.submitted ? "已提交" : r.error || "待核对"]; })));
        }
      } finally { writing = false; }
    }
    await preview();
  }
  function beforeLeave(e) {
    if (dirty) {
      e.preventDefault();
      e.returnValue = "有未保存的修改";
    }
  }
  window.addEventListener("beforeunload", beforeLeave);
  var unbindKeyboard = u.dialogKeyboard(
    function () {
      return drawer;
    },
    function () {
      return close();
    },
  );
  refresh();
  var timer = setInterval(refresh, 5000);
  return {
    beforeLeave: function () {
      return !dirty || window.confirm("有未保存的修改，确认离开？");
    },
    destroy: function () {
      dead = true;
      generation++;
      clearInterval(timer);
      window.removeEventListener("beforeunload", beforeLeave);
      unbindKeyboard();
      close(true);
    },
  };
});
