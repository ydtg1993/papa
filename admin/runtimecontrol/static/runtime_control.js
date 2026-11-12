(function () {
  "use strict";
  var css = document.createElement("style");
  css.textContent =
    ".rt-toolbar{display:flex;gap:10px;align-items:center;flex-wrap:wrap;margin-bottom:18px}.rt-toolbar h2{margin:0 auto 0 0}.rt-tabs{display:flex;gap:8px;border-bottom:1px solid var(--border-color,#ddd);padding-bottom:12px}.rt-tabs [aria-selected=true]{border-color:var(--border-color,#111);color:var(--foreground,#111);background:var(--primary,#FFD83D)}.rt-table{width:100%;border-collapse:collapse;font-variant-numeric:tabular-nums}.rt-table th,.rt-table td{text-align:left;padding:12px 10px;border-bottom:1px solid var(--border-color,#ddd);vertical-align:top}.rt-muted{font-size:12px;opacity:.7}.rt-status{min-height:24px;white-space:pre-wrap}.rt-scroll{overflow:auto}.rt-drawer{position:fixed;right:0;top:0;bottom:0;width:min(640px,100%);box-sizing:border-box;background:var(--card,#fff);color:var(--foreground,#20262c);box-shadow:-5px 0 0 var(--shadow-color,#111);border-left:2px solid var(--border-color,#111);z-index:800;overflow:auto;padding:24px}.rt-field{display:grid;grid-template-columns:1fr 150px;gap:8px;margin:16px 0;align-items:center}.rt-field small{grid-column:1/-1;opacity:.7}.rt-field input[type=number],.rt-field input[type=text],.rt-field select{width:100%;box-sizing:border-box;padding:8px}.rt-preview{white-space:pre-wrap;overflow-wrap:anywhere;max-height:180px;overflow:auto;font-size:12px;border-left:3px solid var(--primary,#FFD83D);padding:12px;background:var(--muted-fill,#f7f7f5)}.rt-kpis{display:flex;gap:30px;flex-wrap:wrap;margin:24px 0}.rt-kpis strong{display:block;font-size:25px}.rt-badge{display:inline-block;border:1px solid currentColor;border-radius:4px;font-size:12px;padding:2px 6px}.rt-members{display:flex;gap:8px;align-items:center;margin:8px 0}.rt-members span{flex:1}@media(max-width:650px){.rt-drawer{padding:16px}.rt-field{grid-template-columns:1fr}.rt-table th,.rt-table td{padding:8px}.rt-toolbar h2{width:100%}}";
  document.head.appendChild(css);
  function el(tag, text, cls) {
    var n = document.createElement(tag);
    if (text !== undefined) n.textContent = text;
    if (cls) n.className = cls;
    return n;
  }
  function button(text, fn) {
    var b = el("button", text, "btn");
    b.type = "button";
    b.onclick = fn;
    return b;
  }
  function clone(v) {
    return JSON.parse(JSON.stringify(v));
  }
  function at(v, path) {
    return path.reduce(function (o, k) {
      return o == null ? undefined : o[k];
    }, v);
  }
  function put(v, path, value) {
    var o = v;
    path.slice(0, -1).forEach(function (k) {
      if (!o[k] || typeof o[k] !== "object") o[k] = {};
      o = o[k];
    });
    if (value === undefined) delete o[path[path.length - 1]];
    else o[path[path.length - 1]] = value;
  }
  function duration(ns) {
    return ns == null
      ? "未采样"
      : (ns / 1e9).toLocaleString(undefined, { maximumFractionDigits: 2 }) +
          " 秒";
  }
  function date(v) {
    return !v || v.indexOf("0001-") === 0 ? "—" : new Date(v).toLocaleString();
  }
  function table(headers, rows) {
    var wrap = el("div", undefined, "rt-scroll"),
      t = el("table", undefined, "rt-table"),
      head = el("tr");
    headers.forEach(function (h) {
      head.appendChild(el("th", h));
    });
    var th = el("thead");
    th.appendChild(head);
    t.appendChild(th);
    var body = el("tbody");
    rows.forEach(function (row) {
      var tr = el("tr");
      row.forEach(function (v) {
        var td = el("td");
        if (v instanceof Node) td.appendChild(v);
        else td.textContent = v == null ? "—" : String(v);
        tr.appendChild(td);
      });
      body.appendChild(tr);
    });
    t.appendChild(body);
    wrap.appendChild(t);
    return wrap;
  }
  async function request(url, options, current) {
    var r = await apiRequest(
      url,
      Object.assign({ rawHTTPError: true, current: current }, options || {}),
    );
    if (!r) throw new Error("操作结果待核对，请刷新服务端状态");
    var d = await r.json();
    if (!r.ok) {
      var e = new Error(d.error || "操作失败");
      e.status = r.status;
      e.data = d;
      throw e;
    }
    return d;
  }
  window.PapaRuntime = {
    el: el,
    button: button,
    clone: clone,
    at: at,
    put: put,
    duration: duration,
    date: date,
    table: table,
    request: request,
    dialogKeyboard: dialogKeyboard,
  };

  function dialogKeyboard(getDrawer, close) {
    function onKey(e) {
      var drawer = getDrawer();
      if (!drawer) return;
      if (e.key === "Escape") {
        if (close()) e.preventDefault();
        return;
      }
      if (e.key !== "Tab") return;
      var nodes = Array.from(
        drawer.querySelectorAll(
          "button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled)",
        ),
      );
      if (!nodes.length) return;
      var first = nodes[0],
        last = nodes[nodes.length - 1],
        active = document.activeElement;
      if (e.shiftKey && (active === first || !drawer.contains(active))) {
        last.focus();
        e.preventDefault();
      } else if (!e.shiftKey && (active === last || !drawer.contains(active))) {
        first.focus();
        e.preventDefault();
      }
    }
    document.addEventListener("keydown", onKey);
    return function () {
      document.removeEventListener("keydown", onKey);
    };
  }

  Papa.page("crawl-control", function (root) {
    var dead = false,
      generation = 0,
      data = null,
      tab = "sites",
      drawer = null,
      dirty = false,
      editor = null,
      writing = false;
    var toolbar = el("div", undefined, "rt-toolbar");
    toolbar.appendChild(el("h2", "采集控制"));
    var status = el("p", "正在读取运行状态…", "rt-status");
    status.setAttribute("role", "status");
    var tabs = el("div", undefined, "rt-tabs"),
      content = el("div"),
      filter = el("input"),
      stateFilter = el("select");
    filter.type = "search";
    filter.placeholder = "筛选站点";
    filter.setAttribute("aria-label", "筛选站点");
    filter.oninput = render;
    stateFilter.setAttribute("aria-label", "筛选站点状态");
    [
      "全部状态",
      "运行",
      "用户暂停",
      "远端等待需处理",
      "远端冷却",
      "等待资源",
      "等待账号",
    ].forEach(function (label, i) {
      var option = el("option", label);
      option.value = i === 0 ? "" : label;
      stateFilter.appendChild(option);
    });
    stateFilter.onchange = render;
    toolbar.append(
      button("调整全局默认", function () {
        openEditor("defaults");
      }),
      button("策略历史", openHistory),
      button("刷新", refresh),
    );
    [
      ["sites", "站点请求"],
      ["resources", "资源控制"],
      ["proxy", "代理健康"],
    ].forEach(function (pair) {
      var b = button(pair[1], function () {
        tab = pair[0];
        render();
      });
      b.dataset.tab = pair[0];
      b.setAttribute("role", "tab");
      tabs.appendChild(b);
    });
    tabs.setAttribute("role", "tablist");
    root.append(toolbar, tabs, status, content);
    function current() {
      return !dead;
    }
    function beforeLeave(e) {
      if (dirty) {
        e.preventDefault();
        e.returnValue = "有未保存的修改";
      }
    }
    window.addEventListener("beforeunload", beforeLeave);
    var unbindKeyboard = dialogKeyboard(
      function () {
        return drawer;
      },
      function () {
        return closeEditor();
      },
    );
    async function refresh() {
      if (writing) return;
      var n = ++generation;
      try {
        var d = await request("/api/runtime-policy", null, function () {
          return !dead && n === generation;
        });
        if (dead || n !== generation) return;
        if (data && d.revision < data.revision) return;
        data = d;
        status.textContent =
          "策略版本 " + d.revision + " · " + date(d.snapshot && d.snapshot.at);
        render();
      } catch (e) {
        if (!dead && n === generation) status.textContent = e.message;
      }
    }
    function closeEditor(force) {
      if (writing && !force) {
        status.textContent = "保存结果尚待核对，请稍候";
        return false;
      }
      if (dirty && !force && !window.confirm("放弃未保存的修改？"))
        return false;
      if (drawer) drawer.remove();
      drawer = null;
      editor = null;
      dirty = false;
      return true;
    }
    function info(title, value) {
      var n = el("div");
      n.append(el("span", title, "rt-muted"), el("strong", value));
      return n;
    }
    function siteReasons(s) {
      var why = [],
        registered = data.sites[s.site] || {};
      if (registered.breaker_paused) why.push("用户暂停");
      if (s.remote_paused) why.push("远端等待需处理");
      if (new Date(s.cooldown_until) > new Date()) why.push("远端冷却");
      if (s.task_waiting || s.browser_waiting) why.push("等待资源");
      if (
        (data.pools || []).some(function (q) {
          return q.key.site === s.site && q.waiting > 0;
        })
      )
        why.push("等待账号");
      return why.length ? why : ["运行"];
    }
    function render() {
      if (!data) return;
      content.replaceChildren();
      Array.from(tabs.children).forEach(function (n) {
        n.setAttribute(
          "aria-selected",
          n.dataset.tab === tab ? "true" : "false",
        );
      });
      var snap = data.snapshot || {},
        sites = snap.sites || [];
      if (tab === "sites") {
        content.append(
          el(
            "p",
            "同站点各阶段、各账号共享请求预算。任务节奏与实际请求限速分别计算。",
            "rt-muted",
          ),
          filter,
          stateFilter,
        );
        var rows = sites
          .filter(function (s) {
            return (
              s.site.indexOf(filter.value) >= 0 &&
              (!stateFilter.value ||
                siteReasons(s).indexOf(stateFilter.value) >= 0)
            );
          })
          .map(function (s) {
            var p = s.policy.request,
              why = siteReasons(s);
            var ops = el("div");
            ops.append(
              button("查看", function () {
                detail(s);
              }),
              button("调整", function () {
                openEditor("site", s.site);
              }),
            );
            return [
              s.site,
              why.join(" / ") || "运行",
              !p.enabled ? "未开启" : p.autothrottle.enabled ? "自动" : "手动",
              s.rate_sampled
                ? Number(s.requests_per_second).toFixed(2)
                : "未采样",
              s.inflight +
                " / " +
                (p.enabled ? p.max_inflight || "不限" : "未启用"),
              duration(s.effective_delay),
              new Date(s.cooldown_until) > new Date()
                ? date(s.cooldown_until)
                : "—",
              s.waiting + s.task_waiting + (s.readiness_waiting || 0),
              ops,
            ];
          });
        content.appendChild(
          table(
            [
              "站点",
              "状态",
              "模式",
              "实际请求/秒",
              "在途 / 上限",
              "有效间隔",
              "冷却至",
              "等待",
              "操作",
            ],
            rows,
          ),
        );
        if (!rows.length)
          content.appendChild(el("p", "暂无匹配的已注册站点。"));
      } else if (tab === "resources") {
        content.append(
          el(
            "p",
            "全局范围 · 影响本进程所有站点。降额只限制新任务，不取消已经运行的任务。",
          ),
        );
        var k = el("div", undefined, "rt-kpis"),
          m = snap.metrics || {};
        k.append(
          info(
            "CPU",
            snap.sample_valid ? Number(m.cpu_percent).toFixed(1) + "%" : "未知",
          ),
          info(
            "内存",
            snap.sample_valid
              ? Number(m.memory_percent).toFixed(1) + "%"
              : "未知",
          ),
          info(
            "可用内存",
            snap.sample_valid
              ? Math.round(m.available_memory_bytes / 1048576) + " MB"
              : "未知",
          ),
        );
        content.append(
          k,
          el(
            "p",
            "来源：" +
              (m.source || "未采样") +
              " · " +
              date(m.sampled_at) +
              " · " +
              (snap.resource_reason || "暂无调节"),
            "rt-muted",
          ),
          button("调整全局资源策略", function () {
            openEditor("resources");
          }),
        );
        content.appendChild(
          table(
            ["执行类别", "使用中", "当前限额", "等待"],
            [
              [
                "任务执行",
                (snap.task || {}).inflight,
                snap.resources && snap.resources.enabled
                  ? (snap.task || {}).limit
                  : "未启用（按物理容量）",
                (snap.task || {}).waiting,
              ],
              [
                "浏览器借用",
                (snap.browser || {}).inflight,
                snap.resources && snap.resources.enabled
                  ? (snap.browser || {}).limit
                  : "未启用（按物理容量）",
                (snap.browser || {}).waiting,
              ],
            ],
          ),
        );
        var physical = data.public_browser_pool;
        content.appendChild(el("h3", "公共浏览器池"));
        if (physical) {
          content.appendChild(
            table(
              ["已创建实例", "已借出", "等待资源", "空闲", "物理容量"],
              [
                [
                  physical.live,
                  physical.borrowed,
                  physical.reserved,
                  physical.idle,
                  physical.capacity,
                ],
              ],
            ),
          );
          content.appendChild(
            el(
              "p",
              "仅统计引擎公共池的受管浏览器实例，不含账号专用池或 Chromium 子进程。",
              "rt-muted",
            ),
          );
        } else {
          content.appendChild(el("p", "未配置公共浏览器池。", "rt-muted"));
        }
        content.appendChild(
          table(
            [
              "站点",
              "权重 / 最低权重",
              "执行中",
              "等待任务",
              "最近服务",
              "操作",
            ],
            sites.map(function (s) {
              return [
                s.site,
                s.policy.weight + " / " + s.policy.min_weight,
                s.task_inflight,
                s.task_waiting,
                date(s.last_service),
                button("调整权重", function () {
                  openEditor("site", s.site);
                }),
              ];
            }),
          ),
        );
      } else {
        content.append(
          el(
            "p",
            "代理标识已脱敏。启用后进入受限探测，不会直接标为健康。",
            "rt-muted",
          ),
          button("调整代理健康策略", function () {
            openEditor("proxy");
          }),
        );
        content.appendChild(
          table(
            [
              "代理标识",
              "状态",
              "连续失败",
              "冷却剩余",
              "在途",
              "固定出口",
              "最近结果",
              "操作",
            ],
            (data.proxies || []).map(function (p) {
              var ops = el("div");
              ops.append(
                button(
                  p.state === "disabled" ? "启用探测" : "停用",
                  function () {
                    proxyAction(
                      p,
                      p.state === "disabled" ? "enable" : "disable",
                    );
                  },
                ),
                button("探测", function () {
                  probeEditor(p);
                }),
              );
              return [
                p.id,
                p.state,
                p.consecutive_failures,
                duration(p.cooldown_remaining),
                p.inflight,
                p.fixed ? "是" : "否",
                p.last_result || "未探测",
                ops,
              ];
            }),
          ),
        );
      }
    }
    function detail(s) {
      if (!closeEditor()) return;
      drawer = el("aside", undefined, "rt-drawer");
      drawer.setAttribute("role", "dialog");
      drawer.setAttribute("aria-label", s.site + "运行详情");
      drawer.append(
        button("关闭", function () {
          closeEditor();
        }),
        el("h2", s.site),
        table(
          ["指标", "读数"],
          [
            ["请求累计", s.requests],
            ["错误累计", s.errors],
            ["限流累计", s.rate_limited],
            ["最近延迟", s.requests ? duration(s.last_latency) : "未采样"],
            ["最近调整", date(s.last_adjustment)],
            ["原因", s.last_reason || "—"],
          ],
        ),
      );
      drawer.appendChild(el("h3", "阶段任务统计"));
      drawer.appendChild(
        table(
          [
            "阶段",
            "工作池处理中",
            "内存队列",
            "完成 / 失败",
            "物理 worker / 执行上限",
          ],
          Object.keys((data.declarations || {})[s.site] || {}).map(
            function (stage) {
              var declared = data.declarations[s.site][stage],
                live = Object.values(data.stages || {}).find(function (v) {
                  return v.Site === s.site && v.Stage === stage;
                }),
                queue = live && live.Queue,
                policy = (s.policy.stages || {})[stage] || {};
              return [
                stage,
                queue ? queue.InProgress : "未创建工作池",
                queue ? queue.QueueLen : "—",
                queue ? queue.Completed + " / " + queue.Failed : "—",
                declared.physical_workers + " / " + policy.max_concurrent,
              ];
            },
          ),
        ),
      );
      drawer.appendChild(
        el(
          "p",
          "工作池处理中包含等待许可的任务；请求量按站点汇总。",
          "rt-muted",
        ),
      );
      (data.coverage || []).forEach(function (t) {
        drawer.appendChild(el("p", t, "rt-muted"));
      });
      if (s.remote_paused)
        drawer.appendChild(
          button("确认按服务器期限继续等待", async function () {
            try {
              data = await request(
                "/api/runtime-remote",
                {
                  method: "POST",
                  headers: { "Content-Type": "application/json" },
                  body: JSON.stringify({
                    site: s.site,
                    revision: data.revision,
                  }),
                },
                current,
              );
              if (!dead) {
                generation++;
                closeEditor(true);
                render();
              }
            } catch (e) {
              if (!dead) status.textContent = e.message;
            }
          }),
        );
      root.appendChild(drawer);
    }
    function probeEditor(p) {
      if (!closeEditor()) return;
      drawer = el("aside", undefined, "rt-drawer");
      var select = el("select");
      Object.keys(data.effective.sites || {}).forEach(function (s) {
        var o = el("option", s);
        o.value = s;
        select.appendChild(o);
      });
      drawer.append(
        el("h2", "探测代理连通性"),
        el("p", "向所选站点的声明首页发送一次 HEAD 请求，遵守站点预算。"),
        select,
        button("执行探测", function () {
          proxyAction(p, "probe", select.value);
        }),
        button("关闭", function () {
          closeEditor();
        }),
      );
      root.appendChild(drawer);
    }
    async function proxyAction(p, action, site) {
      if (writing) return;
      writing = true;
      generation++;
      try {
        await request(
          "/api/proxy-health",
          {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              action: action,
              id: p.id,
              revision: p.version,
              site: site || "",
            }),
          },
          current,
        );
        if (!dead) {
          status.textContent = "代理操作完成";
          writing = false;
          await refresh();
        }
      } catch (e) {
        if (!dead)
          status.textContent = e.message + "；请核对当前状态后再操作。";
      } finally {
        writing = false;
      }
    }

    var requestFields = [
      ["enabled", "启用站点请求控制", "bool"],
      ["max_inflight", "最大请求并发", "number"],
      ["requests_per_second", "每秒速率（0 不限）", "number"],
      ["burst", "突发额度", "number"],
      ["manual_delay", "手动请求间隔", "seconds"],
      ["retry_after_default", "无 Retry-After 时冷却", "seconds"],
      ["retry_after_max", "最大自动等待预算", "seconds"],
      ["autothrottle.enabled", "开启 AutoThrottle", "bool"],
      ["autothrottle.min_delay", "自动最小间隔", "seconds"],
      ["autothrottle.max_delay", "自动最大间隔", "seconds"],
      ["autothrottle.target_concurrency", "自动目标并发", "number"],
      ["autothrottle.smoothing", "平滑系数（0～1）", "number"],
    ];
    var resourceFields = [
      ["enabled", "启用资源控制", "bool"],
      ["sample_interval", "采样间隔", "seconds"],
      ["cpu_high", "CPU 降额阈值（%）", "number"],
      ["cpu_recover", "CPU 恢复阈值（%）", "number"],
      ["high_windows", "持续高负载窗口数", "number"],
      ["memory_high", "内存降额阈值（%）", "number"],
      ["memory_recover", "内存恢复阈值（%）", "number"],
      ["minimum_available_bytes", "最低可用内存（字节）", "number"],
      ["task_max", "全局任务许可上限", "number"],
      ["browser_max", "全局浏览器借用上限", "number"],
      ["shrink_factor", "降额保留比例", "number"],
      ["recovery_step", "每次恢复许可数", "number"],
      ["recovery_interval", "恢复间隔", "seconds"],
    ];
    var proxyFields = [
      ["enabled", "启用代理健康反馈", "bool"],
      ["failure_threshold", "连续失败阈值", "number"],
      ["initial_cooldown", "初始冷却", "seconds"],
      ["max_cooldown", "最大冷却", "seconds"],
      ["max_probes", "半开探测上限", "number"],
      ["recovery_successes", "恢复所需成功数", "number"],
    ];
    function openEditor(kind, site) {
      if (!data || !closeEditor()) return;
      var baseDoc = clone(data.document),
        doc = clone(baseDoc),
        revision = data.revision,
        ops = [],
        fields = [];
      drawer = el("aside", undefined, "rt-drawer");
      drawer.setAttribute("role", "dialog");
      drawer.setAttribute("aria-label", "调整采集策略");
      var headline =
        kind === "site"
          ? site
          : kind === "resources"
            ? "全局资源策略"
            : kind === "proxy"
              ? "代理健康策略"
              : "全局请求默认值";
      drawer.append(
        el("h2", headline),
        el(
          "p",
          "勾选“后台覆盖”才写入该项；取消勾选后继承文件与声明默认值。",
          "rt-muted",
        ),
      );
      var form = el("form");
      form.onsubmit = function (e) {
        e.preventDefault();
      };
      var preview = el("pre", "暂无修改", "rt-preview"),
        message = el("p", undefined, "rt-status");
      message.setAttribute("role", "status");
      var prefix = ["overrides"],
        effective,
        base;
      if (kind === "site") {
        prefix.push("sites", site);
        effective = data.effective.sites[site];
        base = data.base.sites[site];
      } else if (kind === "defaults") {
        prefix.push("defaults");
        effective = data.effective.defaults;
        base = data.base.defaults;
      } else if (kind === "resources") {
        prefix.push("resources");
        effective = data.effective.resources;
        base = data.base.resources;
      } else {
        prefix.push("proxy_health");
        effective = data.proxy_policy;
        base = data.proxy_base || {};
      }
      function field(path, label, type) {
        var full = prefix.concat(path),
          old = at(baseDoc, full),
          value = old === undefined ? at(effective, path) : old,
          row = el("label", undefined, "rt-field"),
          name = el("span", label + (type === "seconds" ? "（秒）" : "")),
          input = el("input"),
          check = el("input");
        check.type = "checkbox";
        check.checked = old !== undefined;
        input.type =
          type === "bool" ? "checkbox" : type === "text" ? "text" : "number";
        if (input.type === "number") {
          input.min = "0";
          input.step = "any";
        }
        if (type === "bool") input.checked = !!value;
        else
          input.value =
            value == null ? "" : type === "seconds" ? value / 1e9 : value;
        input.disabled = !check.checked;
        var enabled = el("span");
        enabled.append(check, document.createTextNode(" 后台覆盖"));
        var baseLabel = format(at(base, path), type),
          effectiveLabel = format(at(effective, path), type);
        if (kind === "site" && path[0] === "stages" && path[2] === "delay") {
          var declaration = at(data.declarations, [site, path[1]]) || {};
          var fileDelay = at(data.file_overrides, ["sites", site].concat(path));
          if (fileDelay === undefined)
            fileDelay = at(data.file_overrides, ["defaults"].concat(path));
          var declared =
            duration(declaration.delay_min) +
            "～" +
            duration(declaration.delay_max);
          baseLabel = fileDelay === undefined ? declared : duration(fileDelay);
          var globalDelay = at(
            data.document,
            ["overrides", "defaults"].concat(path),
          );
          effectiveLabel =
            old !== undefined
              ? duration(old)
              : globalDelay !== undefined
                ? duration(globalDelay)
                : baseLabel;
        }
        row.append(
          name,
          input,
          enabled,
          el(
            "small",
            "文件/声明：" +
              baseLabel +
              " · 生效：" +
              effectiveLabel +
              " · 来源：" +
              (old === undefined ? "文件/声明" : "后台"),
          ),
        );
        fields.push({
          path: full,
          old: old,
          input: input,
          check: check,
          type: type,
        });
        function update() {
          input.disabled = !check.checked;
          rebuild();
        }
        check.onchange = update;
        input.oninput = update;
        form.appendChild(row);
      }
      function format(v, t) {
        if (v == null) return "继承";
        if (t === "bool") return v ? "开启" : "关闭";
        return t === "seconds" ? duration(v) : String(v);
      }
      if (kind === "resources")
        resourceFields.forEach(function (f) {
          field([f[0]], f[1], f[2]);
        });
      else if (kind === "proxy")
        proxyFields.forEach(function (f) {
          field([f[0]], f[1], f[2]);
        });
      else {
        requestFields.forEach(function (f) {
          field(["request"].concat(f[0].split(".")), f[1], f[2]);
        });
        field(["weight"], "站点服务权重", "number");
        field(["min_weight"], "最低服务权重", "number");
        if (kind === "site") {
          field(["pool"], "默认账号池（空为不选池）", "text");
          Object.keys(effective.stages || {}).forEach(function (stage) {
            form.appendChild(el("h3", "阶段 · " + stage));
            field(
              ["stages", stage, "max_concurrent"],
              "有效执行并发（物理上限 " +
                base.stages[stage].max_concurrent +
                "）",
              "number",
            );
            field(["stages", stage, "delay"], "任务间隔", "seconds");
            field(["stages", stage, "pool"], "阶段账号池", "text");
          });
        }
      }
      function rebuild() {
        ops = [];
        fields.forEach(function (f) {
          var v;
          if (f.check.checked)
            v =
              f.type === "bool"
                ? f.input.checked
                : f.type === "text"
                  ? f.input.value
                  : Number(f.input.value) * (f.type === "seconds" ? 1e9 : 1);
          if (JSON.stringify(v) !== JSON.stringify(f.old))
            ops.push({ path: f.path, value: v });
        });
        doc = clone(baseDoc);
        ops.forEach(function (op) {
          put(doc, op.path, op.value);
        });
        dirty = ops.length > 0;
        preview.textContent = ops.length
          ? ops
              .map(function (op) {
                return (
                  op.path.slice(1).join(" / ") +
                  " → " +
                  (op.value === undefined
                    ? "继承默认"
                    : JSON.stringify(op.value))
                );
              })
              .join("\n")
          : "暂无修改";
      }
      var save = button("保存并生效", async function () {
        rebuild();
        if (writing || !dirty) return;
        if (!form.reportValidity()) return;
        writing = true;
        generation++;
        save.disabled = true;
        message.textContent = "正在保存…";
        try {
          var result = await request(
            "/api/runtime-policy",
            {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ revision: revision, document: doc }),
            },
            current,
          );
          if (dead) return;
          data = result;
          closeEditor(true);
          render();
          status.textContent = "策略已生效 · 版本 " + result.revision;
        } catch (e) {
          if (dead) return;
          message.textContent = e.message;
          try {
            var latest = await request("/api/runtime-policy", null, current);
            if (dead) return;
            data = latest;
            if (
              latest.revision === revision + 1 &&
              JSON.stringify(latest.document) === JSON.stringify(doc)
            ) {
              closeEditor(true);
              render();
              status.textContent = "已核对：策略保存成功";
              return;
            }
            message.textContent +=
              "\n服务端版本 " + latest.revision + "；草稿已保留，未自动重试。";
            var reconcile = button("核对最新值并保留本次字段修改", function () {
              var pending = ops.slice();
              baseDoc = clone(latest.document);
              revision = latest.revision;
              fields.forEach(function (f) {
                var own = pending.find(function (op) {
                  return JSON.stringify(op.path) === JSON.stringify(f.path);
                });
                f.old = at(baseDoc, f.path);
                var v = own ? own.value : f.old;
                f.check.checked = v !== undefined;
                f.input.disabled = !f.check.checked;
                if (f.type === "bool") f.input.checked = !!v;
                else
                  f.input.value =
                    v === undefined ? "" : f.type === "seconds" ? v / 1e9 : v;
              });
              message.textContent =
                "已采用最新版本；请核对下方变更后重新保存。";
              rebuild();
              reconcile.remove();
            });
            message.appendChild(reconcile);
            var latestValues = el(
              "pre",
              JSON.stringify(at(latest.document, prefix) || {}, null, 2),
              "rt-preview",
            );
            message.appendChild(latestValues);
          } catch (readError) {
            message.textContent += "\n读取服务端状态失败，结果仍待核对。";
          }
        } finally {
          writing = false;
          save.disabled = false;
        }
      });
      drawer.append(
        form,
        el("h3", "本次变更"),
        preview,
        message,
        save,
        button("取消", function () {
          closeEditor();
        }),
      );
      root.appendChild(drawer);
      editor = { close: closeEditor };
      var first = drawer.querySelector("input");
      if (first) first.focus();
    }
    async function openHistory() {
      if (!data || !closeEditor()) return;
      var pane = el("aside", undefined, "rt-drawer"),
        note = el("p", "正在加载历史…", "rt-status"),
        entries = el("div");
      drawer = pane;
      pane.setAttribute("role", "dialog");
      pane.setAttribute("aria-label", "策略历史");
      pane.append(el("h2", "策略历史"), el("p", "保留最近 100 个版本。恢复选中的字段会生成新版本，其余设置保持当前值。", "rt-muted"), note, entries,
        button("关闭", function () { closeEditor(); }));
      root.appendChild(pane);
      function live() { return !dead && drawer === pane; }
      var listVersion, previewGeneration = 0;
      try {
        var history = await request("/api/runtime-policy/history", null, live);
        if (!live()) return;
        listVersion = history.revision;
        note.textContent = "当前版本 " + listVersion;
        entries.appendChild(table(["版本", "保存时间", "操作者", "操作"], history.history.map(function (row) {
          return [row.revision, date(row.created_at), row.operator || "—", button("查看差异", function () { preview(row.revision); })];
        })));
        if (!history.history.length) entries.appendChild(el("p", "尚无历史记录。升级前的当前设置会在下次保存时归档。"));
      } catch (e) { if (live()) note.textContent = e.message; }
      async function preview(target) {
        if (!live() || writing) return;
        if (dirty && !window.confirm("放弃当前选择并查看另一版本？")) return;
        var revision = listVersion, previewID = ++previewGeneration;
        function previewLive() { return live() && previewID === previewGeneration; }
        try {
          var result = await request("/api/runtime-policy/history", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({action:"preview", revision:revision, target:target})}, previewLive);
          if (!previewLive()) return;
          dirty = false;
          entries.replaceChildren();
          note.textContent = "版本 " + revision + " → 参考版本 " + target + "；只恢复选中的字段。" + (result.warning ? "\n" + result.warning : "");
          var selected = [],
            apply = button("恢复所选字段并生成新版本", restore);
          apply.disabled = true;
          entries.appendChild(table(["选择", "字段", "当前覆盖", "历史覆盖"], result.changes.map(function (change) {
            var check = el("input");
            check.type = "checkbox";
            check.setAttribute("aria-label", "恢复 " + change.path.slice(1).join(" / "));
            check.onchange = function () {
              if (check.checked) selected.push(change.path);
              else selected = selected.filter(function (path) { return path !== change.path; });
              dirty = selected.length > 0;
              apply.disabled = !dirty || writing;
            };
            return [check, change.path.slice(1).join(" / "), change.before_present ? JSON.stringify(change.before) : "继承默认", change.after_present ? JSON.stringify(change.after) : "继承默认"];
          })));
          if (!result.changes.length) entries.appendChild(el("p", "当前覆盖与此版本一致。"));
          entries.append(apply);
          async function restore() {
            if (!live() || writing || !selected.length) return;
            writing = true;
            generation++;
            apply.disabled = true;
            entries.querySelectorAll("input").forEach(function (x) { x.disabled = true; });
            try {
              var saved = await request("/api/runtime-policy/history", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({action:"restore", revision:revision, target:target, paths:selected})}, live);
              if (!live()) return;
              data = saved;
              closeEditor(true);
              render();
              status.textContent = "已恢复所选字段 · 新版本 " + saved.revision;
            } catch (e) {
              if (live()) {
                note.textContent = e.message + "；未自动重试。请刷新并核对当前版本后重新预览。";
                // An ambiguous response must not enable an automatic duplicate save.
                apply.disabled = true;
              }
            } finally { writing = false; }
          }
        } catch (e) { if (previewLive()) note.textContent = e.message + "；请关闭历史窗口后刷新。"; }
      }
    }
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
        closeEditor(true);
      },
    };
  });
})();
