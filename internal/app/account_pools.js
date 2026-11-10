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
        ? "当前账号保持不变，仅在失效或人工停用时选择备用。"
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
          ["invalid", "disabled", "cooling"]
            .map(function (state) {
              return p.accounts.filter(function (m) {
                return m.state === state;
              }).length;
            })
            .join(" / "),
          p.disabled ? "已停用" : p.wait_reason || "可用",
          p.waiting,
          p.history.length
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
          "失效 / 停用 / 冷却",
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
        body.action === "verify"
          ? "已提交重新验证，稍后刷新查看结果。"
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
          "最近检查",
          "冷却至",
          "操作",
        ],
        p.accounts.map(function (m) {
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
            button("重新验证", function () {
              act({
                action: "verify",
                account: m.key,
                revision: m.management.version,
              });
            }),
          );
          return [
            m.key.id,
            m.state + (m.persisted ? "" : "（未保存）"),
            (m.http_ready ? "就绪" : "未验证") +
              " / " +
              (m.browser_ready ? "就绪" : "未验证"),
            m.in_use + " / " + (m.max_concurrent || "不限"),
            m.management.version,
            m.management.reason || m.failure || "—",
            u.date(m.http_configured ? m.last_check : m.last_browser_check),
            u.date(m.management.cooldown_until),
            ops,
          ];
        }),
      ),
    );
    drawer.append(
      el("h3", "切换记录"),
      u.table(
        ["时间", "原账号 → 新账号", "原因", "触发任务", "选择版本"],
        p.history
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
        "切换不会自动重投失败任务。任务补偿仍从错误队列或任务管理入口执行。",
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
        }
      : {
          key: { site: "", name: "" },
          members: [],
          transport: "http",
          disabled: false,
          verify_attempts: 1,
          switch_limit: 16,
          cooldown: 5e9,
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
