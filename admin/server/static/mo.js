/**
 * Papa Monitor — 前台逻辑
 * 由 mo.js 重构而来：保留主题切换，加入监控后台的取数/渲染/设置逻辑；
 * 表格页由 oao 组件（github.com/ydtg1993/oao）渲染，本文件只负责菜单与装配。
 * 顶层函数/变量需保持全局，供 template.html 的内联 onclick 调用。
 */
'use strict';

/* ============ 主题切换 ============ */
    var themeKey = 'papa-theme';
    function getTheme() { return localStorage.getItem(themeKey) || 'light'; }
    function setTheme(t) {
        document.documentElement.setAttribute('data-theme', t);
        localStorage.setItem(themeKey, t);
        var btn = document.getElementById('theme-toggle');
        if (btn) btn.textContent = t === 'dark' ? '☀ 浅色' : '☾ 深色';
    }
    setTheme(getTheme());
    var themeBtn = document.getElementById('theme-toggle');
    if (themeBtn) themeBtn.addEventListener('click', function () {
        setTheme(getTheme() === 'dark' ? 'light' : 'dark');
    });

    /* ============ 基础状态 ============ */
    var KEY_STORAGE = 'papa_monitor_key';
    var MODULE_TITLES = { dashboard: 'Dashboard', queue: '任务队列', queues: '队列治理', tables: '数据', pages: '自定义页', custom: '自定义数据', tokens: '访问令牌', settings: '设置' };
    var currentModule = 'dashboard';

    function loadKey() { return localStorage.getItem(KEY_STORAGE) || ''; }
    function saveKey(k) { localStorage.setItem(KEY_STORAGE, k); }
    function clearKey() { localStorage.removeItem(KEY_STORAGE); }

    function showLogin(showErr) {
        // 可见性挂在 <html> 的 need-login 上（不是遮罩自己的 .hidden）：这样 <head> 里的
        // 内联脚本能在第一帧之前就把状态定好，刷新时不会闪一下登录框
        document.documentElement.classList.add('need-login');
        document.getElementById('err').style.display = showErr ? 'block' : 'none';
        document.getElementById('key').focus();
    }
    function hideLogin() { document.documentElement.classList.remove('need-login'); }
    // 输密钥后除了拉数据，也要重建两个动态菜单 —— 否则首次访问输完密钥，
    // 侧边栏的表格页与自定义页都是空的，要手动刷新才出来
    function submitKey() { saveKey(document.getElementById('key').value); fetchData(); renderNav(); }
    function setStatus(ok, text) {
        document.getElementById('dot').className = ok ? 'dot ok' : 'dot';
        document.getElementById('statusText').textContent = text;
    }

    function switchModule(name) {
        currentModule = name;
        document.querySelectorAll('.module').forEach(function (m) { m.classList.remove('active'); });
        document.getElementById('mod-' + name).classList.add('active');
        document.querySelectorAll('.nav-item').forEach(function (b) {
            b.classList.toggle('active', b.dataset.module === name);
        });
        document.getElementById('moduleTitle').textContent = MODULE_TITLES[name];
        if (name === 'settings') { fetchSettings(); fetchLogs(); }
        if (name === 'tokens') { fetchTokens(); }
    }

    /* ============ 表格（oao 组件） ============ */
    var currentTable = null; // 当前打开的表 key（切站点 tab 时要按新站点重画它）

    /**
     * tableFilterOpts 让表格跟着**当前站点 tab** 走：切到某站再打开任务表，就带着 `site=<该站>` 的
     * 预置筛选打开（oao 会把它填进筛选栏，用户能改能清）。概览与「默认 scope」都不预设 ——
     * 前者本来就是看全部，后者的"site 为空"这个条件在 oao 的查询串里表达不出来（空值会被跳过）。
     */
    function tableFilterOpts() {
        if (currentSite === null || currentSite === '') return { filter: {} };
        return { filter: { site: currentSite } };
    }

    /** 打开某张表：切到表格模块、点亮对应菜单项，再让 oao 渲染 */
    async function openTable(key, label, btn) {
        switchModule('tables');
        currentTable = key;
        document.getElementById('moduleTitle').textContent = label || '数据';
        if (btn) {
            // switchModule 会按 data-module 清空高亮，这里把当前表重新点亮
            document.querySelectorAll('.nav-item').forEach(function (b) { b.classList.remove('active'); });
            btn.classList.add('active');
        }
        try {
            // 第三个参数 = oao 的预置筛选（需要 oao ≥ 带 opts.filter 的那版；老版本会忽略它）
            await Oao.render(document.getElementById('oao-view'), key, tableFilterOpts());
        } catch (e) {
            document.getElementById('oao-view').innerHTML =
                '<div class="empty">表格加载失败：' + esc(e.message) + '</div>';
        }
    }

    /** 按 Group 分组渲染侧边栏的动态菜单 */
    /**
     * 内置表格页的固定菜单：这些表的菜单项写在 template.html 的侧边栏里
     * （框架自带功能，不该混在业务表格的分组里），动态分组里就不重复出。
     * key → 按钮 id；按钮在表没注册时藏起来（比如没开 operation_log）。
     */
    var BUILTIN_TABLES = { operation_log: 'nav-oplog' };

    /** 侧边栏动态菜单：表格页与自定义页**合并**，按 group 首次出现的顺序分组
     *  （与 oao 的 mount 同一约定 —— 分开渲染会让同名的组裂成两段） */
    async function renderNav() {
        var nav = document.getElementById('oao-nav');
        if (!nav) return;

        var tables = [];
        try { tables = (await Oao.list(true)) || []; } catch (e) { tables = []; }
        var pages = [];
        var resp = await apiFetch('/api/pages');
        if (resp) {
            try { pages = (await resp.json()).pages || []; } catch (e) { pages = []; }
        }

        // 固定菜单项：注册了就显示（标题也以表声明为准，省得两处各写一遍），没注册就藏
        Object.keys(BUILTIN_TABLES).forEach(function (key) {
            var btn = document.getElementById(BUILTIN_TABLES[key]);
            if (!btn) return;
            var t = tables.filter(function (x) { return x.key === key; })[0];
            if (t) btn.textContent = t.label;
            btn.classList.toggle('hidden', !t);
        });

        var items = tables.filter(function (t) { return !BUILTIN_TABLES[t.key]; })
            .map(function (t) { return { key: t.key, label: t.label, group: t.group, page: false }; })
            .concat(pages.map(function (p) { return { key: p.key, label: p.label, group: p.group, page: true }; }));
        if (!items.length) { nav.innerHTML = ''; return; }

        var groups = [];
        items.forEach(function (it) {
            var g = it.group || 'General';
            if (groups.indexOf(g) === -1) groups.push(g);
        });
        nav.innerHTML = groups.map(function (g) {
            return '<div class="nav-section">' + esc(g) + '</div>'
                + items.filter(function (it) { return (it.group || 'General') === g; })
                    .map(function (it) {
                        var attr = it.page ? 'data-page' : 'data-oao';
                        return '<button class="nav-item" ' + attr + '="' + esc(it.key) + '">' + esc(it.label) + '</button>';
                    }).join('');
        }).join('');

        nav.querySelectorAll('.nav-item').forEach(function (b) {
            if (b.dataset.page) b.onclick = function () { openPage(b.dataset.page, b.textContent, b); };
            else b.onclick = function () { openTable(b.dataset.oao, b.textContent, b); };
        });
    }

    /* ============ 自定义页（业务用 app.UsePage 注册） ============ */
    /**
     * 页脚本用它注册渲染函数 —— app.UsePage 的 Script 里写：
     *   Papa.page('review', function (el, meta) { el.innerHTML = '…'; });
     * el 是 #page-view；Toast / Dialog / skeletonRows / esc / apiFetch / apiPost 都是全局可用的。
     */
    var Papa = (function () {
        var renderers = {};
        return {
            page: function (key, fn) {
                if (typeof fn === 'function') renderers[key] = fn;
            },
            has: function (key) { return typeof renderers[key] === 'function'; },
            /** 调用某页的渲染函数；没注册返回 false */
            render: function (key, el, meta) {
                var fn = renderers[key];
                if (typeof fn !== 'function') return false;
                fn(el, meta || {});
                return true;
            },
        };
    })();

    /** 打开某个自定义页：切到 pages 模块、点亮菜单项，再交给页脚本渲染 */
    function openPage(key, label, btn) {
        switchModule('pages');
        var title = document.getElementById('moduleTitle');
        if (title) title.textContent = label || '自定义页';
        if (btn) {
            // switchModule 会按 data-module 清空高亮，这里把当前项重新点亮
            document.querySelectorAll('.nav-item').forEach(function (b) { b.classList.remove('active'); });
            btn.classList.add('active');
        }
        var view = document.getElementById('page-view');
        view.innerHTML = '';
        if (!Papa.render(key, view, { key: key, label: label })) {
            view.innerHTML = '<div class="empty">页面 ' + esc(key)
                + ' 没有注册渲染函数：检查 app.UsePage 的 Script 里是否调了 Papa.page("'
                + esc(key) + '", fn)</div>';
        }
    }

    /* ============ UI 基础组件（阶段 0） ============ */

    /** Toast 右下角浮层反馈；容器挂在 body 上，不受模块重绘影响 */
    var Toast = (function () {
        var box = null;
        function ensure() {
            if (!box) {
                box = document.createElement('div');
                box.className = 'toasts';
                document.body.appendChild(box);
            }
            return box;
        }
        return {
            show: function (msg, type, ms) {
                var el = document.createElement('div');
                el.className = 'toast ' + (type || 'info');
                el.textContent = msg;            // textContent：消息内容不解析 HTML
                ensure().appendChild(el);
                setTimeout(function () { el.remove(); }, ms || 3200);
            },
            ok: function (msg, ms) { this.show(msg, 'ok', ms); },
            err: function (msg, ms) { this.show(msg, 'err', ms); },
            info: function (msg, ms) { this.show(msg, 'info', ms); }
        };
    })();

    /**
     * Dialog 模态弹窗。open 返回 Promise：
     *   - 点击某个按钮 -> resolve(该按钮的 value)
     *   - 点遮罩 / 按 Esc / 点取消 -> resolve(null)
     * 用法：Dialog.alert({...})、Dialog.confirm({...}) 是常用封装。
     */
    var Dialog = (function () {
        var mask = null, elTitle = null, elBody = null, elActions = null;
        var settle = null;   // 当前 Promise 的 resolve

        function ensure() {
            if (mask) return;
            mask = document.createElement('div');
            mask.className = 'dlg-mask';
            mask.innerHTML = '<div class="dlg" role="dialog" aria-modal="true">'
                + '<h3></h3><div class="dlg-body"></div><div class="dlg-actions"></div></div>';
            mask.addEventListener('mousedown', function (e) { if (e.target === mask) close(null); });
            document.body.appendChild(mask);
            elTitle = mask.querySelector('h3');
            elBody = mask.querySelector('.dlg-body');
            elActions = mask.querySelector('.dlg-actions');
            document.addEventListener('keydown', function (e) {
                if (e.key === 'Escape' && mask.classList.contains('open')) close(null);
            });
        }

        function close(value) {
            if (!mask || !mask.classList.contains('open')) return;
            mask.classList.remove('open');
            var done = settle;
            settle = null;
            if (done) done(value);
        }

        function open(opts) {
            ensure();
            opts = opts || {};
            mask.querySelector('.dlg').classList.toggle('danger', !!opts.danger);
            elTitle.textContent = opts.title || '提示';
            elBody.innerHTML = opts.body || '';        // 调用方负责转义
            elActions.innerHTML = '';
            var actions = opts.actions || [{ label: '知道了', value: 'ok' }];
            actions.forEach(function (a) {
                var b = document.createElement('button');
                b.className = 'btn' + (a.tone === 'danger' ? ' danger' : '');
                b.textContent = a.label;
                b.onclick = function () {
                    // 表单类弹窗用 onClick 先校验：返回 false 表示"先别关"（关闭会清掉表单）
                    if (a.onClick && a.onClick() === false) return;
                    close(a.value);
                };
                elActions.appendChild(b);
            });
            mask.classList.add('open');
            var first = elActions.querySelector('.btn');
            if (first) first.focus();
            return new Promise(function (resolve) { settle = resolve; });
        }

        return {
            open: open,
            /** alert 只有一个「知道了」按钮，resolve(true) */
            alert: function (opts) {
                var actions = [{ label: (opts && opts.okLabel) || '知道了', value: true }];
                return open(Object.assign({}, opts, { actions: actions }));
            },
            /** confirm 确认/取消，resolve 布尔 */
            confirm: function (opts) {
                opts = opts || {};
                return open({
                    title: opts.title || '请确认',
                    body: opts.body || '',
                    danger: opts.danger,
                    actions: [
                        { label: opts.cancelLabel || '取消', value: false },
                        { label: opts.okLabel || '确定', value: true, tone: opts.danger ? 'danger' : '' }
                    ]
                }).then(function (v) { return v === true; });
            },
            close: function () { close(null); }
        };
    })();

    /** skeleton 生成 n 行占位，用于表格加载态 */
    function skeletonRows(cols, n) {
        var html = '';
        for (var i = 0; i < n; i++) {
            html += '<tr class="skel-row">';
            for (var c = 0; c < cols; c++) html += '<td><span class="skel"></span></td>';
            html += '</tr>';
        }
        return html;
    }


    /** showSkeleton 表格加载占位；cols 不传则沿用当前表头列数 */
    function showSkeleton(id, cols) {
        var el = document.getElementById(id);
        if (!el) return;
        var n = cols || el.querySelectorAll('thead th').length || 6;
        el.innerHTML = '<table><tbody>' + skeletonRows(n, 5) + '</tbody></table>';
    }
    function esc(s) {
        return String(s).replace(/[&<>"']/g, function (c) {
            return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
        });
    }
    /** tipAttr 生成 data-tip 属性（内容已转义），用于超长内容悬停查看全文 */
    function tipAttr(text) {
        var s = (text === null || text === undefined) ? '' : String(text);
        return s ? ' data-tip="' + esc(s) + '"' : '';
    }
    /* ============ 格式化 ============ */
    function fmtDuration(ns) {
        if (ns == null || isNaN(ns)) return '-';
        if (ns < 1e6) return (ns / 1e3).toFixed(2) + 'µs';
        if (ns < 1e9) return (ns / 1e6).toFixed(2) + 'ms';
        return (ns / 1e9).toFixed(2) + 's';
    }
    function fmtBytes(b) {
        if (b == null) return '-';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = 0, v = b;
        while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
        return v.toFixed(i === 0 ? 0 : 1) + ' ' + units[i];
    }
    function pct(p) { return p == null ? 0 : Math.min(100, Math.max(0, p)); }
    function jsonVal(v) {
        if (v === null || v === undefined) return String(v);
        if (typeof v === 'object') return JSON.stringify(v);
        return String(v);
    }
    function bar(cls, p) { return '<div class="bar ' + cls + '"><span style="width:' + p + '%"></span></div>'; }

    /* ============ 渲染 ============ */
    function renderSystem(s) {
        var el = document.getElementById('dash-sys');
        if (!s) { el.innerHTML = '<div class="empty">无系统指标</div>'; return; }
        el.innerHTML =
            '<div class="card"><div class="label">CPU</div><div class="value">' + (s.cpu_percent || 0).toFixed(1) + '%</div>' + bar('cpu', pct(s.cpu_percent)) + '</div>'
            + '<div class="card"><div class="label">内存</div><div class="value">' + fmtBytes(s.mem_used) + '</div><div class="sub">共 ' + fmtBytes(s.mem_total) + '</div>' + bar('ram', pct(s.mem_percent)) + '</div>'
            + '<div class="card"><div class="label">磁盘</div><div class="value">' + fmtBytes(s.disk_used) + '</div><div class="sub">共 ' + fmtBytes(s.disk_total) + '</div>' + bar('disk', pct(s.disk_percent)) + '</div>';
    }
    function renderDirs(dirs) {
        var el = document.getElementById('dash-dirs');
        var keys = Object.keys(dirs || {});
        if (keys.length === 0) { el.innerHTML = '<div class="empty">未配置 monitor_dirs</div>'; return; }
        el.innerHTML = keys.map(function (name) {
            var d = dirs[name];
            return '<div class="card"><div class="label">' + esc(name) + '</div><div class="value">' + fmtBytes(d.size) + '</div><div class="sub">' + esc(d.path || '') + '</div></div>';
        }).join('');
    }
    function stageTiles(q) {
        q = q || {};
        return '<div class="tiles">'
            + '<div class="tile queued"><div class="k">排队</div><div class="v">' + q.queue_len + '</div></div>'
            + '<div class="tile running"><div class="k">执行中</div><div class="v">' + q.in_progress + '</div></div>'
            + '<div class="tile done"><div class="k">完成</div><div class="v">' + q.completed + '</div></div>'
            + '<div class="tile failed"><div class="k">错误</div><div class="v">' + q.failed + '</div></div>'
            + '<div class="tile submitted"><div class="k">已提交</div><div class="v">' + q.submitted + '</div></div>'
            + '</div>';
    }
    function renderQueueSummary(stages) {
        var el = document.getElementById('dash-queue');
        var names = Object.keys(stages || {});
        if (names.length === 0) { el.innerHTML = '<div class="empty">暂无运行中的阶段</div>'; return; }
        el.innerHTML = names.map(function (name) {
            return '<div class="stage"><h3>' + esc(name) + '</h3>' + stageTiles(stages[name].queue) + '</div>';
        }).join('');
    }
    function renderQueue(stages) {
        var el = document.getElementById('queue-stages');
        var names = Object.keys(stages || {});
        if (names.length === 0) { el.innerHTML = '<div class="empty">暂无运行中的阶段</div>'; return; }
        el.innerHTML = names.map(function (name) {
            var st = stages[name];
            var g = st.global || {};
            var workers = st.workers || {};
            var wrows = Object.keys(workers).map(function (id) {
                var w = workers[id];
                return '<tr><td>#' + id + '</td><td>' + w.TotalTasks + '</td><td>' + w.FailedTasks
                    + '</td><td>' + fmtDuration(w.TotalTime) + '</td><td>' + fmtDuration(w.MaxTime)
                    + '</td><td>' + fmtDuration(w.MinTime) + '</td></tr>';
            }).join('');
            return '<div class="stage"><h3>' + esc(name) + '</h3>' + stageTiles(st.queue)
                + '<div class="muted">累计任务 ' + g.TotalTasks + ' · 累计失败 ' + g.TotalFailed
                + ' · 平均 ' + fmtDuration(g.AvgTime) + ' · 最大 ' + fmtDuration(g.MaxTime) + ' · 最小 ' + fmtDuration(g.MinTime) + '</div>'
                + '<div style="margin-top:12px"><div class="table-wrap"><table><tr><th>Worker</th><th>任务</th><th>失败</th><th>总耗时</th><th>最大</th><th>最小</th></tr>'
                + wrows + '</table></div></div></div>';
        }).join('');
    }
    /* ---- 治理队列（错误队列 + 轮询队列，**两条都按站点各占一行**） ---- */
    // 两条队列的命名是同一套：默认 scope 是历史名字（`error_queue` / `repeat_queue`），
    // 命名站点是 `<队列>:<站点>`（站点那一行只处理该站的任务，所以行里要带上站点）。
    var QUEUE_KINDS = {
        error_queue: { label: '错误队列', desc: '失败任务重投', path: '/api/errorqueue/process', key: 'processed', done: '已重新投递' },
        repeat_queue: { label: '轮询队列', desc: '周期任务重投', path: '/api/repeatqueue/process', key: 'repolled', done: '已重投' }
    };
    // parseQueue 拆出队列名里的"哪条队列 + 哪个站点"；不是治理队列返回 null
    //（快照里还有别的键，认不出来就不往面板上放）。
    function parseQueue(name) {
        var base = name, site = '', i = name.indexOf(':');
        if (i >= 0) { base = name.slice(0, i); site = name.slice(i + 1); }
        var kind = QUEUE_KINDS[base];
        return kind ? { kind: kind, base: base, site: site } : null;
    }
    function queueMeta(name) {
        var p = parseQueue(name);
        if (!p) return null;
        return {
            label: p.kind.label + (p.site === '' ? '（默认 scope）' : (' · 站点 ' + p.site)),
            desc: p.kind.desc,
            url: p.kind.path + (p.site === '' ? '' : '?site=' + encodeURIComponent(p.site)),
            key: p.kind.key, done: p.kind.done
        };
    }
    // 面板上的顺序：错误队列在前、轮询队列在后（按队列名字典序），各自"默认 scope → 站点名字典序"
    function queueKeys(data) {
        var names = Object.keys(data || {}).filter(function (n) { return parseQueue(n) !== null; });
        names.sort(function (a, b) {
            var pa = parseQueue(a), pb = parseQueue(b);
            if (pa.base !== pb.base) return pa.base < pb.base ? -1 : 1;
            if (pa.site === '' || pb.site === '') return pa.site === '' ? -1 : 1;
            return pa.site < pb.site ? -1 : (pa.site > pb.site ? 1 : 0);
        });
        return names;
    }
    function timeValid(d) { return !isNaN(d.getTime()) && d.getFullYear() >= 2000; }
    function agoSeconds(iso) {
        var d = new Date(iso);
        return (!iso || !timeValid(d)) ? null : Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
    }
    function fmtSpan(sec) {
        if (sec < 60) return sec + 's';
        if (sec < 3600) return Math.floor(sec / 60) + 'm' + (sec % 60) + 's';
        return Math.floor(sec / 3600) + 'h' + Math.floor((sec % 3600) / 60) + 'm';
    }
    function fmtAgo(iso) {
        var s = agoSeconds(iso);
        if (s === null) return '从未';
        if (s < 60) return s + ' 秒前';
        if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
        if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
        return Math.floor(s / 86400) + ' 天前';
    }
    function absTime(iso) {
        var d = new Date(iso);
        return (!iso || !timeValid(d)) ? '从未' : d.toLocaleString();
    }
    function queueStatus(q) {
        var title = q.last_error ? ' title="上次错误：' + esc(q.last_error) + '"' : '';
        if (q.running) {
            var s = agoSeconds(q.started_at);
            return '<span class="badge running"' + title + '>运行中</span>'
                + '<div class="muted">已执行 ' + (s === null ? '-' : fmtSpan(s)) + '</div>';
        }
        if (!q.enabled) return '<span class="badge off"' + title + '>已停用</span>';
        return '<span class="badge ok"' + title + '>空闲</span>';
    }
    function renderQueues(queues) {
        var el = document.getElementById('queue-gov');
        var data = queues || {};
        el.innerHTML = '<table class="qtable"><thead><tr>'
            + '<th>队列</th><th>状态</th><th>上次执行</th><th>处理量</th><th>待处理</th><th>操作</th>'
            + '</tr></thead><tbody>'
            + queueKeys(data).map(function (name) {
                var m = queueMeta(name);
                var q = data[name] || {};
                var ran = q.runs > 0;
                var backlog = q.backlog || 0;
                return '<tr>'
                    + '<td><b>' + esc(m.label) + '</b><div class="muted">' + esc(name) + ' · ' + esc(m.desc) + '</div></td>'
                    + '<td>' + queueStatus(q) + '</td>'
                    + '<td title="完成于 ' + esc(absTime(q.last_finish_at)) + '">' + esc(fmtAgo(q.last_finish_at))
                    + '<div class="muted">' + (ran ? ('耗时 ' + fmtDuration(q.last_duration)) : '尚未执行') + '</div></td>'
                    + '<td>' + (q.running ? (q.run_processed || 0) : (ran ? q.last_processed : 0))
                    + '<div class="muted">' + (q.running ? '本轮已处理' : '上次处理') + ' · 累计 ' + (q.total_processed || 0) + '</div></td>'
                    + '<td>' + (backlog > 0 ? '<span class="badge warn">' + backlog + '</span>' : '0')
                    + '<div class="muted">' + esc(fmtAgo(q.backlog_at)) + '采样</div></td>'
                    // 队列名现在是运行期才知道的（按站点拆），所以走 data-* + 从 dataset 取 ——
                    // 与熔断那个「恢复」按钮同一套写法：内联属性里只有固定代码，运行期值不拼进 JS 字符串
                    + '<td><button class="btn" style="margin-top:0" data-queue="' + esc(name)
                    + '" onclick="triggerQueue(this.dataset.queue)">立即执行</button></td>'
                    + '</tr>';
            }).join('')
            + '</tbody></table>';
    }
    async function triggerQueue(name) {
        var m = queueMeta(name);
        if (!m) return;
        var el = document.getElementById('queue-gov-msg');
        el.textContent = m.label + '处理中...';
        var resp = await apiPost(m.url, {});
        el.textContent = '';
        if (!resp || !resp.ok) {
            Toast.err(m.label + '处理失败');
            return;
        }
        var data = await resp.json();
        Toast.ok(m.label + '：' + m.done + ' ' + (data[m.key] || 0) + ' 个任务');
        fetchData();
    }
    function renderCustom(custom) {
        var el = document.getElementById('custom');
        var keys = Object.keys(custom || {});
        if (keys.length === 0) { el.innerHTML = '<div class="empty">暂无自定义数据（fetcher 里调 engine.RecordMetric 写入）</div>'; return; }
        el.innerHTML = keys.map(function (k) {
            return '<div class="row"><div class="k">' + esc(k) + '</div><div class="v">' + esc(jsonVal(custom[k])) + '</div></div>';
        }).join('');
    }
    /* ============ 站点 Tab（多站点时按站点把面板分开） ============ */
    // currentSite：null = 概览（全部）；'' = 默认 scope（任务 site 为空串那一档）；其余 = 站点键。
    // 用 null 当"全部"而不是 ''，就是为了能单独看"默认 scope"那一档。
    var currentSite = null;

    function siteLabel(key) { return key === '' ? '默认 scope' : key; }

    // siteView 按当前站点过滤阶段与队列（概览返回全部）。
    // 队列那一侧靠队列名编码的站点（`<队列>:<站点>`）—— 错误队列与轮询队列**都是**按站点拆的，
    // 两条都要跟着走（只认轮询队列的话，站点 Tab 下错误队列会整块消失）。
    function siteView(stages, queues) {
        if (currentSite === null) return { stages: stages, queues: queues };
        var outStages = {}, outQueues = {};
        Object.keys(stages || {}).forEach(function (name) {
            if ((stages[name].site || '') === currentSite) outStages[name] = stages[name];
        });
        Object.keys(queues || {}).forEach(function (name) {
            var p = parseQueue(name);
            if (p && p.site === currentSite) outQueues[name] = queues[name];
        });
        return { stages: outStages, queues: outQueues };
    }

    function renderSiteTabs(data) {
        var el = document.getElementById('site-tabs');
        if (!el) return;
        var sites = (data && data.sites) || {};
        var keys = Object.keys(sites).sort(function (a, b) {
            if (a === '' || b === '') return a === '' ? 1 : -1; // 默认 scope 排最后
            return a < b ? -1 : (a > b ? 1 : 0);
        });
        if (keys.length === 0) { // 还没有站点快照（老引擎 / 没播种）：整排隐藏，不占位
            el.hidden = true;
            document.getElementById('site-meta').hidden = true;
            return;
        }
        if (currentSite !== null && keys.indexOf(currentSite) < 0) currentSite = null; // 站点没了回概览
        el.innerHTML = ['<button class="site-tab' + (currentSite === null ? ' active' : '') +
            '" data-site="__all__">概览（全部）</button>'].concat(keys.map(function (k) {
                return '<button class="site-tab' + (currentSite === k ? ' active' : '') +
                    '" data-site="' + esc(k) + '">' + esc(siteLabel(k)) + '</button>';
            })).join('');
        // 与队列按钮同一套：data-* 带运行期值，内联属性里只有固定代码（站点名不拼进 JS 字符串）
        el.querySelectorAll('.site-tab').forEach(function (b) {
            b.onclick = function () { switchSite(b.dataset.site === '__all__' ? null : b.dataset.site); };
        });
        el.hidden = false;
        renderSiteMeta(sites);
    }

    function renderSiteMeta(sites) {
        var meta = document.getElementById('site-meta');
        if (!meta) return;
        var stat = currentSite === null ? null : sites[currentSite];
        if (!stat) { meta.hidden = true; meta.innerHTML = ''; return; }
        var parts = [
            '<b>' + esc(siteLabel(stat.key)) + '</b>',
            esc(stat.base_url || '—'),
            (stat.stage_count || 0) + ' 个阶段',
            stat.auto_repeat ? '自动轮询：开' : '自动轮询：<b>关</b>（只能手动触发）',
            '累计轮询 ' + (stat.repeat_total || 0),
            '待轮询 ' + (stat.repeat_backlog || 0),
            '上次轮询 ' + esc(fmtAgo(stat.last_repeat_at)),
            stat.breaker_paused
                ? '<b>熔断已闸住</b>（' + esc(fmtAgo(stat.breaker_paused_at)) + '）'
                : '熔断未闸住'
        ];
        if (stat.last_repeat_error) parts.push('最近错误：' + esc(stat.last_repeat_error));
        parts.push('<span class="muted">任务表在左侧菜单，进去后用「站点」筛选看本站的任务</span>');
        meta.innerHTML = parts.join(' ｜ ');
        meta.hidden = false;
    }

    function switchSite(site) {
        currentSite = site;
        if (lastData) renderAll(lastData); // 手上有快照就直接重画，不必等下一次轮询
        // 正开着某张表时，也按新站点重画它 —— 切站点 tab 就该连表里的筛选一起跟着换
        if (currentModule === 'tables' && currentTable) {
            Oao.render(document.getElementById('oao-view'), currentTable, tableFilterOpts());
        }
    }

    var lastData = null;

    function renderAll(data) {
        lastData = data;
        renderSiteTabs(data);
        var view = siteView(data.stages, data.queues);
        renderSystem(data.system);
        renderDirs(data.system && data.system.dirs);
        renderQueueSummary(view.stages);
        renderQueue(view.stages);
        renderQueues(view.queues);
        renderCustom(data.custom);
        // 键名是**复数**：服务端 monitor.go 写的是 resp["breakers"]（数组，每站一条）。
        // 这里曾经写成 data.breaker，于是横幅永远拿不到数据、（list || []）把它吞成空数组，
        // 一个字都不报 —— 多站熔断横幅其实一直没显示过。
        // **不过滤**：横幅是告警，切到哪个站点 tab 都该看得见（切走了反而看不见出事的是哪个站）。
        renderBreaker(data.breakers);
    }

    /* ---- 熔断横幅（多站时逐 scope 一行） ---- */
    // 每个站点一把闸门（引擎按 scope 分组）：哪一站被闸住、为什么、什么时候，以及**只放行那一站**的按钮；
    // 两个以上被闸住时额外给一个「全部恢复」。状态跟着 /api/monitor 每轮刷新一起来（不额外发请求），
    // 所以别的标签页 / 别的操作人触发的熔断也能看到。
    function renderBreaker(list) {
        var el = document.getElementById('breaker-banner');
        if (!el) return;
        var paused = (list || []).filter(function (b) { return b && b.paused; });
        if (!paused.length) { el.hidden = true; el.innerHTML = ''; return; }

        var html = paused.map(function (b) {
            var at = b.paused_at ? new Date(b.paused_at) : null;
            var when = (at && timeValid(at)) ? at.toLocaleString() : '';
            var where = b.site ? ('站点 ' + b.site) : '默认 scope';
            return '<div class="breaker-row">' +
                '<span class="breaker-text">抓取已暂停（熔断）｜' + esc(where) +
                (b.stage ? '｜阶段 ' + esc(b.stage) : '') +
                '｜' + esc(fmtSpan(b.window / 1e9)) + ' 窗口内 ' + (b.failures || 0) +
                ' 次终态失败，阈值 ' + (b.threshold || 0) +
                (when ? '｜暂停于 ' + esc(when) : '') +
                '。处理完后点右侧按钮恢复。</span>' +
                '<button class="btn" data-scope="' + esc(b.site || '') +
                '" onclick="resumeCrawling(this.dataset.scope)">恢复</button>' +
                '</div>';
        }).join('');
        if (paused.length > 1) {
            html += '<div class="breaker-row"><span class="breaker-text">共 ' + paused.length +
                ' 个 scope 被闸住</span>' +
                '<button class="btn" onclick="resumeCrawling()">全部恢复</button></div>';
        }
        el.innerHTML = html; // 每一段都过了 esc()
        el.hidden = false;
    }
    // scope 省略 = 全部放行（多站时后台横幅上的「全部恢复」）。
    async function resumeCrawling(scope) {
        var resp = await apiPost('/api/breaker/resume', scope ? { scope: scope } : {});
        if (!resp || !resp.ok) { Toast.err('恢复失败'); return; }
        var data = null;
        try { data = await resp.json(); } catch (e) { /* 拿不到 body 不影响放行结果 */ }
        // resumed=false 说明本来就没闸住（比如另一个人先点了）—— 说清楚，别报个含糊的成功
        if (data && data.resumed === false) Toast.info('当前不在暂停态');
        else Toast.ok(scope ? ('已恢复 ' + scope) : '已恢复抓取');
        fetchData();
    }

    /* ============ 取数 ============ */
    async function apiFetch(url) {
        var headers = {};
        var key = loadKey();
        if (key) headers['Authorization'] = 'Bearer ' + key;
        var resp;
        try { resp = await fetch(url, { headers: headers }); }
        catch (e) { setStatus(false, '连接失败'); return null; }
        if (resp.status === 401) { clearKey(); setStatus(false, '需要密钥'); showLogin(true); return null; }
        if (!resp.ok) return null;
        return resp;
    }
    async function apiPost(url, body) {
        var headers = { 'Content-Type': 'application/json' };
        var key = loadKey();
        if (key) headers['Authorization'] = 'Bearer ' + key;
        var resp;
        try { resp = await fetch(url, { method: 'POST', headers: headers, body: JSON.stringify(body || {}) }); }
        catch (e) { setStatus(false, '连接失败'); return null; }
        if (resp.status === 401) { clearKey(); setStatus(false, '需要密钥'); showLogin(true); return null; }
        if (!resp.ok) return null;
        return resp;
    }
    async function fetchData() {
        var key = loadKey();
        var headers = {};
        if (key) headers['Authorization'] = 'Bearer ' + key;
        var resp;
        try { resp = await fetch('/api/monitor', { headers: headers }); }
        catch (e) { setStatus(false, '连接失败'); return; }
        if (resp.status === 401) { clearKey(); setStatus(false, '需要密钥'); showLogin(true); return; }
        if (!resp.ok) { setStatus(false, 'HTTP ' + resp.status); return; }
        var data = await resp.json();
        hideLogin();
        setStatus(true, '已连接');
        document.getElementById('refreshTime').textContent = '刷新于 ' + new Date().toLocaleTimeString();
        renderAll(data);
    }

    /* ============ 设置 ============ */
    async function fetchSettings() {
        var resp = await apiFetch('/api/settings');
        if (!resp) return;
        var data = await resp.json();
        document.getElementById('wl-input').value = (data.whitelist || []).join('\n');
        document.getElementById('wl-file-info').textContent = data.whitelist_file
            ? ('持久化文件：' + data.whitelist_file + (data.has_whitelist_file ? '（已存在）' : '（首次保存时创建）'))
            : '未配置 whitelist_file：改动仅本次运行有效';
    }
    async function saveWhitelist() {
        var lines = document.getElementById('wl-input').value.split('\n').map(function (s) { return s.trim(); }).filter(Boolean);
        var resp = await apiPost('/api/settings/whitelist', { whitelist: lines });
        document.getElementById('wl-msg').textContent = resp && resp.ok ? '已保存' : '';
        if (resp && resp.ok) Toast.ok('白名单已保存');
        else Toast.err('白名单保存失败');
    }
    /* ============ 访问令牌 ============ */
    /**
     * 这一页是后台自带的模块，不是 oao 表格页：表格组件的动作只回 {"status":"ok"}、不回数据，
     * 而「新增」必须把服务端生成的明文令牌交给操作人看一次（库里只存 sha256，过后无从显示）。
     */
    async function fetchTokens() {
        var el = document.getElementById('token-table');
        if (!el) return;
        el.innerHTML = '<table><tbody>' + skeletonRows(7, 3) + '</tbody></table>';
        var resp = await apiFetch('/api/tokens');
        if (!resp) { el.innerHTML = '<div class="empty">读取失败，请稍后重试</div>'; return; }
        var data = await resp.json();
        renderTokens(data.tokens || []);
    }

    function renderTokens(rows) {
        var el = document.getElementById('token-table');
        if (!rows.length) {
            el.innerHTML = '<div class="empty">还没有任何访问令牌 —— 此时 /api/* 对白名单内的来源完全开放。'
                + '点上面的「新增令牌」建一把。</div>';
            return;
        }
        var html = '<table><thead><tr><th>ID</th><th>操作人</th><th>状态</th><th>备注</th>'
            + '<th>创建时间</th><th>更新时间</th><th>操作</th></tr></thead><tbody>';
        rows.forEach(function (t) {
            html += '<tr>'
                + '<td>' + t.id + '</td>'
                + '<td>' + esc(t.operator) + '</td>'
                + '<td>' + (t.enabled
                    ? '<span class="badge ok">启用中</span>'
                    : '<span class="badge off">已停用</span>') + '</td>'
                + '<td>' + esc(t.note || '') + '</td>'
                + '<td>' + fmtTime(t.created_at) + '</td>'
                + '<td>' + fmtTime(t.updated_at) + '</td>'
                + '<td><div class="row-actions">'
                + (t.enabled
                    ? '<button class="btn" data-token-off="' + t.id + '">停用</button>'
                    : '<button class="btn" data-token-on="' + t.id + '">启用</button>')
                + '<button class="btn danger" data-token-del="' + t.id + '">删除</button>'
                + '</div></td></tr>';
        });
        el.innerHTML = html + '</tbody></table>';

        el.querySelectorAll('[data-token-off]').forEach(function (b) {
            b.onclick = function () { setTokenEnabled(Number(b.dataset.tokenOff), false); };
        });
        el.querySelectorAll('[data-token-on]').forEach(function (b) {
            b.onclick = function () { setTokenEnabled(Number(b.dataset.tokenOn), true); };
        });
        el.querySelectorAll('[data-token-del]').forEach(function (b) {
            b.onclick = function () { removeToken(Number(b.dataset.tokenDel)); };
        });
    }

    /** 后端给的是 RFC3339（带纳秒），浏览器 Date 只认到毫秒：多出来的小数位先截掉 */
    function fmtTime(s) {
        if (!s) return '';
        var d = new Date(String(s).replace(/(\.\d{3})\d+/, '$1'));
        return isNaN(d.getTime()) ? String(s) : d.toLocaleString();
    }

    /** 新增：表单 → 服务端生成 → 弹窗把明文显示一次 */
    async function openNewToken() {
        var p = Dialog.open({
            title: '新增访问令牌',
            body: '<div class="field"><label for="tk-operator">操作人</label>'
                + '<input id="tk-operator" placeholder="谁用这把令牌，比如 张三" autocomplete="off"></div>'
                + '<div class="field"><label for="tk-note">备注</label>'
                + '<input id="tk-note" placeholder="可选，比如 运维机" autocomplete="off"></div>'
                + '<div class="hint">令牌由服务端生成，<b>只显示这一次</b>；库里只存 sha256，过后找不回来。</div>'
                + '<div class="errtext" id="tk-err"></div>',
            actions: [
                { label: '取消', value: null },
                {
                    label: '创建', value: 'ok',
                    onClick: function () {
                        if (!document.getElementById('tk-operator').value.trim()) {
                            document.getElementById('tk-err').textContent = '操作人不能为空';
                            document.getElementById('tk-operator').focus();
                            return false; // 别关，让用户补上
                        }
                        return true;
                    }
                }
            ]
        });
        var operator = document.getElementById('tk-operator');
        var note = document.getElementById('tk-note');

        if (await p !== 'ok') return;
        var data = await tokenWrite('/api/tokens', { operator: operator.value, note: note.value });
        if (!data || !data.token) return;

        var token = data.token;
        await Dialog.open({
            title: '令牌（只显示这一次）',
            body: '<p>给「' + esc(operator.value.trim()) + '」的访问令牌：</p>'
                + '<div class="secret-box">' + esc(token) + '</div>'
                + '<p class="hint">请立刻存好：库里只有 sha256，关掉这个窗口就再也看不到明文了。</p>',
            actions: [
                { label: '复制', value: 'copy', onClick: function () { copyText(token); return false; } },
                { label: '我已存好', value: null }
            ]
        });
        fetchTokens();
    }

    async function setTokenEnabled(id, enabled) {
        if (!enabled) {
            var ok = await Dialog.confirm({
                title: '停用访问令牌',
                body: '<p>停用后这把令牌立刻失效（可以再启用）。</p>'
                    + '<p>如果它是最后一把启用中的令牌，后台将对所有人拒绝，'
                    + '那种情况只能用 <code>papa token add</code> 救回来。</p>',
                danger: true, okLabel: '停用'
            });
            if (!ok) return;
        }
        if (!await tokenWrite('/api/tokens/enabled', { id: id, enabled: enabled })) return;
        Toast.ok(enabled ? '已启用' : '已停用');
        fetchTokens();
    }

    async function removeToken(id) {
        var ok = await Dialog.confirm({
            title: '删除访问令牌',
            body: '<p>删除后无法恢复（这条令牌与它的哈希一起删掉）。</p><p>只是暂时不用的话，改用「停用」。</p>',
            danger: true, okLabel: '删除'
        });
        if (!ok) return;
        if (!await tokenWrite('/api/tokens/remove', { id: id })) return;
        Toast.ok('已删除');
        fetchTokens();
    }

    /**
     * 令牌页的写操作。刻意不用 apiPost：那个"失败即 null"会吞掉服务端的话术，
     * 而这里的失败原因要让人看见（"操作人不能为空"、"该令牌状态已变，请刷新后重试"）。
     */
    async function tokenWrite(url, body) {
        var headers = { 'Content-Type': 'application/json' };
        var key = loadKey();
        if (key) headers['Authorization'] = 'Bearer ' + key;
        var resp;
        try { resp = await fetch(url, { method: 'POST', headers: headers, body: JSON.stringify(body || {}) }); }
        catch (e) { Toast.err('连接失败'); return null; }
        if (resp.status === 401) { clearKey(); setStatus(false, '需要密钥'); showLogin(true); return null; }
        var data = {};
        try { data = await resp.json(); } catch (e) { /* 没 body 也能报状态码 */ }
        if (!resp.ok) { Toast.err(data.error || ('操作失败 HTTP ' + resp.status)); return null; }
        return data;
    }

    /** 复制到剪贴板；明文令牌是长串随机字符，靠手抄容易错一位 */
    function copyText(text) {
        if (navigator.clipboard && window.isSecureContext) {
            navigator.clipboard.writeText(text).then(
                function () { Toast.ok('已复制到剪贴板'); },
                function () { Toast.err('复制失败，请手动选中'); });
            return;
        }
        // 非安全上下文（http + 局域网 IP）拿不到 navigator.clipboard，退回选中 + execCommand
        var ta = document.createElement('textarea');
        ta.value = text;
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.select();
        var ok = false;
        try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
        document.body.removeChild(ta);
        if (ok) Toast.ok('已复制到剪贴板'); else Toast.err('复制失败，请手动选中');
    }
    // 关停是高危操作：要人在场，所以让操作人把自己的访问令牌再输一遍。
    // 服务端会拿它去查令牌表（`/api/settings/shutdown` 的 body 里带 token），输错返回 403。
    // 用 Dialog.open 而不是 confirm —— 需要一个输入框，并且要在点「退出」时校验非空。
    function askShutdownToken() {
        var typed = '';
        return Dialog.open({
            title: '优雅退出',
            body: '<p>确定要优雅退出爬虫进程吗？</p>'
                + '<p>引擎会等待在途任务结束，随后关闭浏览器池与数据库连接。</p>'
                + '<p class="hint">为避免误触，请填写你自己的访问令牌确认身份。</p>'
                + '<input class="input" id="shutdown-token" type="password" autocomplete="off" placeholder="访问令牌">',
            actions: [
                { label: '取消', value: false },
                {
                    label: '退出', tone: 'danger', value: true,
                    onClick: function () {
                        var el = document.getElementById('shutdown-token');
                        typed = el ? el.value.trim() : '';
                        if (!typed) { Toast.err('请填写访问令牌'); return false; } // false = 先别关，留着输入框
                        return true;
                    }
                }
            ]
        }).then(function (ok) { return ok ? typed : ''; });
    }

    async function doShutdown() {
        var token = await askShutdownToken();
        if (!token) return;

        document.getElementById('shutdown-msg').textContent = '正在关闭...';
        // 不用 apiPost：它把非 2xx 一律吞成 null，而这里正需要把服务端那句"访问令牌不正确"显示出来
        var resp;
        try {
            var headers = { 'Content-Type': 'application/json' };
            var key = loadKey();
            if (key) headers['Authorization'] = 'Bearer ' + key;
            resp = await fetch('/api/settings/shutdown', {
                method: 'POST', headers: headers, body: JSON.stringify({ token: token })
            });
        } catch (e) {
            Toast.err('连接失败');
            return;
        }
        if (resp.ok) {
            Toast.info('已触发优雅退出');
            return;
        }
        if (resp.status === 401) { clearKey(); setStatus(false, '需要密钥'); showLogin(true); return; }
        var msg = '';
        try { msg = (await resp.text()).trim(); } catch (e) { msg = ''; }
        Toast.err(msg || ('退出请求失败（HTTP ' + resp.status + '）'));
    }

    /* ============ 日志 ============ */
    async function fetchLogs() {
        var resp = await apiFetch('/api/logs');
        if (!resp) return;
        var data = await resp.json();
        var el = document.getElementById('log-list');
        var files = data.files || [];
        el.innerHTML = '';
        if (files.length === 0) { el.innerHTML = '<div class="hint">日志目录为空</div>'; return; }
        files.forEach(function (f) {
            var row = document.createElement('div');
            row.className = 'log-row';
            var nm = document.createElement('span');
            nm.className = 'nm'; nm.textContent = f.name;
            var sz = document.createElement('span');
            sz.className = 'sz'; sz.textContent = fmtBytes(f.size);
            var a = document.createElement('a');
            a.textContent = '下载';
            a.onclick = function () { downloadLog(f.name); };
            row.appendChild(nm); row.appendChild(sz); row.appendChild(a);
            el.appendChild(row);
        });
    }
    async function downloadFile(url) {
        var headers = {};
        var key = loadKey();
        if (key) headers['Authorization'] = 'Bearer ' + key;
        var resp = await fetch(url, { headers: headers });
        if (resp.status === 401) { clearKey(); showLogin(true); return; }
        if (!resp.ok) { alert('下载失败 HTTP ' + resp.status); return; }
        var blob = await resp.blob();
        var cd = resp.headers.get('Content-Disposition') || '';
        var name = 'logs.zip';
        var m = /filename="?([^";]+)"?/.exec(cd);
        if (m) name = m[1];
        var a = document.createElement('a');
        a.href = URL.createObjectURL(blob);
        a.download = name;
        document.body.appendChild(a);
        a.click();
        a.remove();
        URL.revokeObjectURL(a.href);
    }
    async function downloadLog(name) { await downloadFile('/api/logs/download?file=' + encodeURIComponent(name)); }
    async function downloadLogs() { await downloadFile('/api/logs/download'); }

    /* ============ 启动 ============ */
    document.getElementById('key').addEventListener('keydown', function (e) { if (e.key === 'Enter') submitKey(); });

    // 表格动作的防重复提交由 oao 组件自己做（runAction 里的 inFlightActions，
    // key 用「表 + 动作 + 主键」）—— v1.2.3 及以前没有，v1.2.4 起有。
    // 宿主不用再包一层；服务端的条件更新始终是最终防线。

    // oao 表格组件：复用同一套访问密钥；接口前缀与静态资源由 papa 挂载
    Oao.init({
        base: '/api/oao',
        headers: function () {
            var key = loadKey();
            return key ? { 'Authorization': 'Bearer ' + key } : {};
        },
        // 密钥失效时和其它接口保持一致：清掉旧密钥、弹登录，而不是只在表格区留一行错误
        // （返回 false 表示没换成新凭据，组件就把 401 如实抛出来）
        onUnauthorized: function () {
            clearKey();
            setStatus(false, '需要密钥');
            showLogin(true);
            return false;
        }
    });

    // 有本地凭据就直接进（遮罩的状态 <head> 的内联脚本已经在首帧前定好了，这里兜底：
    // 万一那段脚本没了，也不会变成"没凭据却不弹登录框"）；没凭据则弹出并聚焦输入框
    if (loadKey()) hideLogin(); else showLogin(false);

    fetchData();
    renderNav();
    setInterval(fetchData, 3000);
