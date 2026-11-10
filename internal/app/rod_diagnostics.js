Papa.page("browser-diagnostics", function (root) {
  "use strict";
  var u = window.PapaRuntime,
    el = u.el,
    button = u.button,
    dead = false,
    busy = false,
    seq = 0,
    data = null,
    drawer = null,
    dirty = false;
  var target = window.PapaIdentityTarget || { site: "", kind: "", key: "" };
  window.PapaIdentityTarget = null;
  var status = el("p", "正在读取身份档案…", "rt-status"),
    tables = el("div"),
    report = el("textarea"),
    scope = el("p", undefined, "rt-muted");
  status.setAttribute("role", "status");
  report.readOnly = true;
  report.style.cssText =
    "width:100%;height:40vh;box-sizing:border-box;font:12px ui-monospace,monospace";
  report.setAttribute("aria-label", "浏览器诊断报告");
  var toolbar = el("div", undefined, "rt-toolbar");
  toolbar.append(
    el("h2", "浏览器诊断"),
    button("启动诊断", startup),
    button("创建档案", function () {
      editProfile(null);
    }),
    button("设置身份绑定", function () {
      editBinding(target);
    }),
    button("刷新", function () {
      refresh(false);
    }),
  );
  var copy = button("复制报告", function () {
    copyText(report.value);
  });
  copy.disabled = true;
  root.append(
    toolbar,
    el(
      "p",
      "先保存稳定的身份档案，再为账号或业务会话绑定。新版本不会自动替换已有绑定；本地检查不验证目标站的认证或反爬通过率。",
    ),
    scope,
    status,
    tables,
    report,
    copy,
  );
  function current() {
    return !dead;
  }
  function setScope() {
    scope.textContent =
      target.kind === "account"
        ? "检查环境：账号 " +
          target.site +
          " / " +
          target.key +
          " 的浏览器配置（不载入凭据）"
        : "检查环境：公共浏览器池配置（独立临时实例）";
  }
  setScope();
  function ref(r) {
    return r ? r.id + " · v" + r.version : "未绑定";
  }
  function same(a, b) {
    return a && b && a.site === b.site && a.kind === b.kind && a.key === b.key;
  }
  function binding(s) {
    return ((data && data.bindings) || []).find(function (b) {
      return same(b.subject, s);
    });
  }
  async function refresh(quiet) {
    if (busy) return;
    var n = ++seq;
    try {
      var d = await u.request("/api/browser-identities", null, function () {
        return !dead && n === seq;
      });
      if (dead || n !== seq) return;
      data = d;
      render();
      if (!quiet)
        status.textContent = "档案版本固定；绑定修订号与账号管理版本独立。";
    } catch (e) {
      if (!dead && n === seq) status.textContent = e.message;
    }
  }
  function close(force) {
    if (busy && !force) return false;
    if (dirty && !force && !window.confirm("放弃未保存的修改？")) return false;
    if (drawer) drawer.remove();
    drawer = null;
    dirty = false;
    return true;
  }
  var unkey = u.dialogKeyboard(function () {
    return drawer;
  }, close);
  function editor(title) {
    if (busy || !close()) return null;
    drawer = el("aside", undefined, "rt-drawer");
    drawer.setAttribute("role", "dialog");
    drawer.setAttribute("aria-label", title);
    drawer.append(el("h2", title));
    root.append(drawer);
    return drawer;
  }
  function field(box, label, value, type, change) {
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
    box.append(row);
    return x;
  }
  async function write(body, done) {
    if (busy) return;
    busy = true;
    var frozen = [];
    if (drawer)
      drawer.querySelectorAll("input,select,textarea,button").forEach(function (node) {
        frozen.push([node, node.disabled]);
        node.disabled = true;
      });
    seq++;
    status.textContent =
      body.action === "reverify"
        ? "等待旧使用结束并复验…"
        : body.action === "diagnose"
          ? "正在执行本地配置一致性检查…"
          : "正在保存…";
    try {
      var d = await u.request(
        "/api/browser-identities",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        },
        current,
      );
      if (dead) return;
      if (done) done(d.result);
      status.textContent = "操作完成。";
    } catch (e) {
      if (!dead) {
        status.textContent = e.message + "；未自动重试。";
        if (drawer) {
          drawer.append(
            el(
              "p",
              e.message + "；草稿已保留，刷新列表不会覆盖草稿。",
              "rt-status",
            ),
          );
          if (e.status === 409)
            drawer.append(
              button("核对最新版本", async function () {
                await refresh(true);
                if (drawer)
                  drawer.append(
                    el(
                      "pre",
                      JSON.stringify(
                        {
                          profiles: data && data.profiles,
                          bindings: data && data.bindings,
                        },
                        null,
                        2,
                      ),
                      "rt-preview",
                    ),
                  );
              }),
            );
        }
      }
    } finally {
      frozen.forEach(function (item) {
        if (item[0].isConnected) item[0].disabled = item[1];
      });
      busy = false;
      if (!dead) await refresh(true);
    }
  }
  async function startup() {
    if (busy) return;
    busy = true;
    status.textContent = "正在诊断浏览器启动…";
    try {
      var r = await apiRequest("/api/browser-diagnostics", {
        method: "POST",
        rawHTTPError: true,
        current: current,
      });
      if (!r) throw Error("结果尚未确认，请查看系统日志");
      if (!r.ok) throw Error(await r.text());
      var d = await r.json();
      if (!dead) {
        report.value = JSON.stringify(d, null, 2);
        copy.disabled = false;
        status.textContent = d.ok ? "启动诊断通过" : "启动诊断发现问题";
      }
    } catch (e) {
      if (!dead) status.textContent = e.message;
    } finally {
      busy = false;
    }
  }
  function diagnose(p) {
    if (target.kind === "account" && target.site !== p.site) {
      status.textContent = "所选账号与档案必须属于同一站点";
      return;
    }
    write(
      {
        action: "diagnose",
        reference: { site: p.site, id: p.id, version: p.version },
        subject:
          target.kind === "account"
            ? target
            : { site: p.site, kind: "", key: "" },
      },
      function (result) {
        report.value = JSON.stringify(result, null, 2);
        copy.disabled = false;
      },
    );
  }
  function render() {
    if (!data) return;
    tables.replaceChildren();
    tables.append(
      el("h3", "身份档案"),
      u.table(
        ["站点 / 档案", "版本", "语言 / 时区", "适配器", "操作"],
        (data.profiles || []).map(function (p) {
          var ops = el("div");
          ops.append(
            button("查看", function () {
              report.value = JSON.stringify(p, null, 2);
              copy.disabled = false;
            }),
            button("追加版本", function () {
              editProfile(p);
            }),
            button("配置一致性检查", function () {
              diagnose(p);
            }),
          );
          return [
            p.site + " / " + p.id,
            p.version,
            p.languages.join(", ") + " / " + p.timezone,
            p.adapter.enabled
              ? p.adapter.id + " v" + p.adapter.version
              : "关闭",
            ops,
          ];
        }),
      ),
    );
    if (!(data.profiles || []).length)
      tables.append(
        el(
          "p",
          "尚未创建身份档案。未绑定的页面继续使用原有浏览器设置。",
          "rt-muted",
        ),
      );
    tables.append(
      el("h3", "账号与会话绑定"),
      u.table(
        ["主体", "活动档案", "待生效档案", "修订 / 使用数", "状态", "操作"],
        (data.bindings || []).map(function (b) {
          var ops = el("div");
          ops.append(
            button("改绑", function () {
              editBinding(b.subject);
            }),
          );
          if (b.active || b.pending)
            ops.append(
              button("复验并生效", function () {
                write({
                  action: "reverify",
                  subject: b.subject,
                  expected: b.revision,
                });
              }),
            );
          if (b.pending)
            ops.append(
              button("取消改绑", function () {
                write({
                  action: "cancel",
                  subject: b.subject,
                  expected: b.revision,
                });
              }),
            );
          return [
            b.subject.site +
              " / " +
              (b.subject.kind === "account" ? "账号" : "会话") +
              " / " +
              b.subject.key,
            ref(b.active),
            ref(b.pending),
            b.revision + " / " + b.in_use,
            b.verifying
              ? "正在复验"
              : {
                  active: b.verified ? "已生效" : "待复验",
                  pending: "待生效",
                  failed: "复验失败",
                  unbound: "未绑定",
                }[b.state] || b.state,
            ops,
          ];
        }),
      ),
    );
    tables.append(
      el(
        "p",
        "改绑后新任务等待；旧任务自然结束。点击“复验并生效”完成切换。失败时可修正后复验或取消改绑。",
        "rt-muted",
      ),
    );
    tables.append(
      el("h3", "最近本地检查"),
      u.table(
        ["时间", "档案", "环境", "结果"],
        (data.reports || []).slice(0, 20).map(function (r) {
          return [
            u.date(r.at),
            r.reference.site + " / " + ref(r.reference),
            r.subject.kind === "account" ? "账号 " + r.subject.key : "公共池",
            (r.ok ? "一致" : "发现差异或未验证") +
              (r.environment_changed ? " · 浏览器环境已变化" : ""),
          ];
        }),
      ),
    );
  }
  function editProfile(old) {
    if (!data) return;
    var box = editor(old ? "追加档案版本" : "创建档案");
    if (!box) return;
    var p = old
      ? u.clone(old)
      : {
          site: target.site || "",
          id: "",
          version: 1,
          schema_version: 1,
          ua_policy: "native-chromium",
          languages: ["zh-CN", "zh"],
          locale: "zh-CN",
          timezone: "Asia/Shanghai",
          viewport: { width: 1280, height: 720 },
          screen: { width: 1920, height: 1080 },
          device_scale_factor: 1,
          touch: false,
          max_touch_points: 0,
          adapter: { enabled: false, id: "webdriver", version: "1" },
        };
    var latest = old
      ? Math.max.apply(
          null,
          data.profiles
            .filter(function (x) {
              return x.site === old.site && x.id === old.id;
            })
            .map(function (x) {
              return x.version;
            }),
        )
      : 0;
    p.version = latest + 1;
    box.append(
      el(
        "p",
        "UA 和 Client Hints 由实际 Chromium 派生。此处不指定其它操作系统或浏览器内核。",
        "rt-muted",
      ),
    );
    field(box, "站点", p.site, "text", function (v) {
      p.site = v;
    }).disabled = !!old;
    field(box, "档案 ID", p.id, "text", function (v) {
      p.id = v;
    }).disabled = !!old;
    var version = el(
      "p",
      "新版本 " + p.version + "（基于版本 " + latest + "）",
    );
    box.append(version);
    field(box, "语言（逗号分隔）", p.languages.join(","), "text", function (v) {
      p.languages = v.split(",").map(function (x) {
        return x.trim();
      });
      p.locale = p.languages[0];
    });
    field(box, "时区", p.timezone, "text", function (v) {
      p.timezone = v;
    });
    [
      ["窗口宽", "viewport", "width"],
      ["窗口高", "viewport", "height"],
      ["屏幕宽", "screen", "width"],
      ["屏幕高", "screen", "height"],
    ].forEach(function (f) {
      field(box, f[0], p[f[1]][f[2]], "number", function (v) {
        p[f[1]][f[2]] = v;
      });
    });
    field(box, "DPR", p.device_scale_factor, "number", function (v) {
      p.device_scale_factor = v;
    }).step = "0.1";
    var points;
    field(box, "触摸", p.touch, "checkbox", function (v) {
      p.touch = v;
      p.max_touch_points = v ? Math.max(1, p.max_touch_points) : 0;
      points.value = p.max_touch_points;
    });
    points = field(
      box,
      "最大触摸点",
      p.max_touch_points,
      "number",
      function (v) {
        p.max_touch_points = v;
      },
    );
    field(
      box,
      "启用 webdriver v1 适配器",
      p.adapter.enabled,
      "checkbox",
      function (v) {
        p.adapter = { enabled: v, id: "webdriver", version: "1" };
      },
    );
    box.append(
      el(
        "p",
        "适配器仅处理 webdriver；字体、Canvas、WebGL 只诊断，不随机化。",
        "rt-muted",
      ),
    );
    box.append(
      button("保存新版本", function () {
        write({ action: "profile", expected: latest, profile: p }, function () {
          close(true);
        });
      }),
      button("采用已核对的最新版本号", function () {
        var all = (data.profiles || []).filter(function (x) {
          return x.site === p.site && x.id === p.id;
        });
        latest = all.reduce(function (n, x) {
          return Math.max(n, x.version);
        }, 0);
        p.version = latest + 1;
        version.textContent =
          "新版本 " + p.version + "（基于版本 " + latest + "；草稿保留）";
      }),
      button("关闭", function () {
        close();
      }),
    );
    box.querySelector("input:not(:disabled)").focus();
  }
  function editBinding(subject) {
    if (!data) return;
    var box = editor("设置身份绑定");
    if (!box) return;
    var s = {
        site: subject.site || "",
        kind: subject.kind || "account",
        key: subject.key || "",
      },
      expected = 0;
    field(box, "站点", s.site, "text", function (v) {
      s.site = v;
      update();
    });
    var row = el("label", undefined, "rt-field"),
      kind = el("select");
    [
      ["account", "账号"],
      ["session", "业务会话"],
    ].forEach(function (x) {
      var o = el("option", x[1]);
      o.value = x[0];
      kind.append(o);
    });
    kind.value = s.kind;
    kind.onchange = function () {
      dirty = true;
      s.kind = kind.value;
      update();
    };
    row.append(el("span", "主体类型"), kind);
    box.append(row);
    field(box, "账号 ID / 稳定会话键", s.key, "text", function (v) {
      s.key = v;
      update();
    });
    box.append(
      el(
        "p",
        "会话键由业务生成并持久保存；重建 Session 仍复用同一键。此处不保存 Cookie，也不自动生成会话 ID。",
        "rt-muted",
      ),
    );
    var select = el("select"),
      selectRow = el("label", undefined, "rt-field"),
      revision = el("p");
    selectRow.append(el("span", "目标档案版本"), select);
    box.append(selectRow, revision);
    function update() {
      var b = binding(s);
      expected = b ? b.revision : 0;
      revision.textContent = "基于绑定修订 " + expected;
      select.replaceChildren();
      (data.profiles || [])
        .filter(function (p) {
          return p.site === s.site;
        })
        .forEach(function (p) {
          var o = el("option", ref(p));
          o.value = JSON.stringify({
            site: p.site,
            id: p.id,
            version: p.version,
          });
          select.append(o);
        });
    }
    update();
    select.onchange = function () {
      dirty = true;
    };
    box.append(
      button("保存为待生效", function () {
        if (!select.value) {
          status.textContent = "请先为此站点创建档案";
          return;
        }
        write(
          {
            action: "stage",
            subject: s,
            reference: JSON.parse(select.value),
            expected: expected,
          },
          function () {
            close(true);
          },
        );
      }),
      button("采用已核对的最新修订", function () {
        var b = binding(s);
        expected = b ? b.revision : 0;
        revision.textContent = "基于绑定修订 " + expected + "（草稿保留）";
      }),
      button("用此账号配置检查", function () {
        if (s.kind !== "account" || !s.site || !s.key) {
          status.textContent = "请填写账号站点和 ID";
          return;
        }
        target = u.clone(s);
        setScope();
        close(true);
      }),
      button("使用公共池检查", function () {
        target = { site: s.site, kind: "", key: "" };
        setScope();
        close(true);
      }),
      button("关闭", function () {
        close();
      }),
    );
    box.querySelector("input").focus();
  }
  function beforeUnload(e) {
    if (dirty || busy) {
      e.preventDefault();
      e.returnValue = "";
    }
  }
  window.addEventListener("beforeunload", beforeUnload);
  refresh(false);
  return {
    canLeave: function () {
      return !busy && (!dirty || window.confirm("放弃未保存的修改？"));
    },
    destroy: function () {
      dead = true;
      seq++;
      unkey();
      window.removeEventListener("beforeunload", beforeUnload);
      close(true);
    },
  };
});
