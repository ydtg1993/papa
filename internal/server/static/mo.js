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
    var MODULE_TITLES = { dashboard: 'Dashboard', queue: '任务队列', queues: '队列治理', tables: '数据', custom: '自定义数据', settings: '设置' };
    var currentModule = 'dashboard';

    function loadKey() { return localStorage.getItem(KEY_STORAGE) || ''; }
    function saveKey(k) { localStorage.setItem(KEY_STORAGE, k); }
    function clearKey() { localStorage.removeItem(KEY_STORAGE); }

    function showLogin(showErr) {
        document.getElementById('login').classList.remove('hidden');
        document.getElementById('err').style.display = showErr ? 'block' : 'none';
        document.getElementById('key').focus();
    }
    function hideLogin() { document.getElementById('login').classList.add('hidden'); }
    function submitKey() { saveKey(document.getElementById('key').value); fetchData(); }
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
    }

    /* ============ 表格（oao 组件） ============ */
    /** 打开某张表：切到表格模块、点亮对应菜单项，再让 oao 渲染 */
    async function openTable(key, label, btn) {
        switchModule('tables');
        document.getElementById('moduleTitle').textContent = label || '数据';
        if (btn) {
            // switchModule 会按 data-module 清空高亮，这里把当前表重新点亮
            document.querySelectorAll('.nav-item').forEach(function (b) { b.classList.remove('active'); });
            btn.classList.add('active');
        }
        try {
            await Oao.render(document.getElementById('oao-view'), key);
        } catch (e) {
            document.getElementById('oao-view').innerHTML =
                '<div class="empty">表格加载失败：' + esc(e.message) + '</div>';
        }
    }

    /** 按 Group 分组渲染侧边栏的动态菜单 */
    async function renderTableNav() {
        var nav = document.getElementById('oao-nav');
        var tables;
        try { tables = await Oao.list(true); }
        catch (e) { nav.innerHTML = ''; return; }

        var groups = [];
        tables.forEach(function (t) {
            var g = t.group || 'General';
            if (groups.indexOf(g) === -1) groups.push(g);
        });
        nav.innerHTML = groups.map(function (g) {
            return '<div class="nav-section">' + esc(g) + '</div>'
                + tables.filter(function (t) { return (t.group || 'General') === g; })
                    .map(function (t) {
                        return '<button class="nav-item" data-oao="' + esc(t.key) + '">' + esc(t.label) + '</button>';
                    }).join('');
        }).join('');

        nav.querySelectorAll('.nav-item').forEach(function (b) {
            b.onclick = function () { openTable(b.dataset.oao, b.textContent, b); };
        });
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
                b.onclick = function () { close(a.value); };
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
    /* ---- 治理队列（error/recover/repeat） ---- */
    var QUEUE_META = {
        error_queue: { label: '错误队列', desc: '失败任务重投', url: '/api/errorqueue/process', key: 'processed', done: '已重新投递' },
        recover_queue: { label: '恢复队列', desc: '卡死任务恢复', url: '/api/recoverqueue/process', key: 'recovered', done: '已恢复' },
        repeat_queue: { label: '轮询队列', desc: '周期任务重投', url: '/api/repeatqueue/process', key: 'repolled', done: '已重投' }
    };
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
            + Object.keys(QUEUE_META).map(function (name) {
                var m = QUEUE_META[name];
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
                    + '<td><button class="btn" style="margin-top:0" onclick="triggerQueue(\'' + esc(name) + '\')">立即执行</button></td>'
                    + '</tr>';
            }).join('')
            + '</tbody></table>';
    }
    async function triggerQueue(name) {
        var m = QUEUE_META[name];
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
    function renderAll(data) {
        renderSystem(data.system);
        renderDirs(data.system && data.system.dirs);
        renderQueueSummary(data.stages);
        renderQueue(data.stages);
        renderQueues(data.queues);
        renderCustom(data.custom);
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
        document.getElementById('secret-file-info').textContent = data.auth_key_file
            ? ('密钥文件：' + data.auth_key_file + (data.has_secret_file ? '（已存在）' : '（不存在）'))
            : '未配置密钥文件：重新生成的密钥仅本次运行有效';
    }
    async function saveWhitelist() {
        var lines = document.getElementById('wl-input').value.split('\n').map(function (s) { return s.trim(); }).filter(Boolean);
        var resp = await apiPost('/api/settings/whitelist', { whitelist: lines });
        document.getElementById('wl-msg').textContent = resp && resp.ok ? '已保存' : '';
        if (resp && resp.ok) Toast.ok('白名单已保存');
        else Toast.err('白名单保存失败');
    }
    async function regenSecret() {
        var ok = await Dialog.confirm({
            title: '重新生成密钥',
            body: '<p>当前登录态会立即切换到新密钥，旧密钥失效。</p><p>未配置密钥文件时，重启后新密钥也会丢失。</p>',
            danger: true, okLabel: '生成'
        });
        if (!ok) return;
        var resp = await apiPost('/api/settings/secret', {});
        if (!resp || !resp.ok) { Toast.err('生成密钥失败'); return; }
        var data = await resp.json();
        saveKey(data.key);
        var box = document.getElementById('secret-result');
        box.classList.remove('hidden');
        box.innerHTML = '新密钥：<br><b>' + esc(data.key) + '</b><br><br>已自动更新登录态；请妥善保存，未配置密钥文件时重启会失效。';
        Toast.ok('已生成新密钥');
    }
    async function doShutdown() {
        var ok = await Dialog.confirm({
            title: '优雅退出',
            body: '<p>确定要优雅退出爬虫进程吗？</p><p>引擎会等待在途任务结束，随后关闭浏览器池与数据库连接。</p>',
            danger: true, okLabel: '退出'
        });
        if (!ok) return;
        document.getElementById('shutdown-msg').textContent = '正在关闭...';
        var resp = await apiPost('/api/settings/shutdown', {});
        if (resp && resp.ok) Toast.info('已触发优雅退出');
        else Toast.err('退出请求失败');
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

    // oao 表格组件：复用同一套访问密钥；接口前缀与静态资源由 papa 挂载
    Oao.init({
        base: '/api/oao',
        headers: function () {
            var key = loadKey();
            return key ? { 'Authorization': 'Bearer ' + key } : {};
        }
    });

    fetchData();
    renderTableNav();
    setInterval(fetchData, 3000);
