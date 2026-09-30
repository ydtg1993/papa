/**
 * Papa Monitor — 前台逻辑
 * 由 mo.js 重构而来：保留主题切换，加入监控后台的取数/渲染/设置/数据浏览逻辑。
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
    var MODULE_TITLES = { dashboard: 'Dashboard', queue: '任务队列', tasks: '任务', data: '数据浏览', custom: '自定义数据', settings: '设置' };
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
        else if (name === 'data') { loadDataModels(); }
        else if (name === 'tasks') { loadTasks(); }
    }

    /* ============ 格式化 ============ */
    function esc(s) {
        return String(s).replace(/[&<>"']/g, function (c) {
            return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
        });
    }
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
    function renderCustom(custom) {
        var el = document.getElementById('custom');
        var keys = Object.keys(custom || {});
        if (keys.length === 0) { el.innerHTML = '<div class="empty">暂无自定义数据（fetcher 里调 engine.RecordMetric 写入）</div>'; return; }
        el.innerHTML = keys.map(function (k) {
            return '<div class="row"><div class="k">' + esc(k) + '</div><div class="v">' + esc(jsonVal(custom[k])) + '</div></div>';
        }).join('');
    }
    function renderStageOptions(stages) {
        var sel = document.getElementById('task-stage');
        var cur = sel.value;
        var names = Object.keys(stages || {});
        sel.innerHTML = '<option value="">全部阶段</option>'
            + names.map(function (n) { return '<option value="' + esc(n) + '">' + esc(n) + '</option>'; }).join('');
        if (cur) sel.value = cur;
    }
    function renderAll(data) {
        renderSystem(data.system);
        renderDirs(data.system && data.system.dirs);
        renderQueueSummary(data.stages);
        renderQueue(data.stages);
        renderCustom(data.custom);
        renderStageOptions(data.stages);
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
        document.getElementById('wl-msg').textContent = resp && resp.ok ? '已保存' : '保存失败';
    }
    async function regenSecret() {
        var resp = await apiPost('/api/settings/secret', {});
        if (!resp || !resp.ok) return;
        var data = await resp.json();
        saveKey(data.key);
        var box = document.getElementById('secret-result');
        box.classList.remove('hidden');
        box.innerHTML = '新密钥：<br><b>' + esc(data.key) + '</b><br><br>已自动更新登录态；请妥善保存，未配置密钥文件时重启会失效。';
    }
    async function doShutdown() {
        if (!confirm('确定要优雅退出爬虫进程吗？')) return;
        document.getElementById('shutdown-msg').textContent = '正在关闭...';
        await apiPost('/api/settings/shutdown', {});
    }
    async function processErrorQueue() {
        document.getElementById('errorqueue-msg').textContent = '处理中...';
        var resp = await apiPost('/api/errorqueue/process', {});
        if (!resp || !resp.ok) {
            document.getElementById('errorqueue-msg').textContent = '处理失败';
            return;
        }
        var data = await resp.json();
        document.getElementById('errorqueue-msg').textContent = '已重新投递 ' + data.processed + ' 个失败任务';
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

    /* ============ 数据浏览 / 任务 ============ */
    var DataBrowser = { model: null, mode: 'data', page: 1, size: 20, search: '', sort: '', filter: {} };

    function dataQ() {
        var q = '/api/data/' + encodeURIComponent(DataBrowser.model)
            + '?page=' + DataBrowser.page + '&size=' + DataBrowser.size;
        if (DataBrowser.search) q += '&search=' + encodeURIComponent(DataBrowser.search);
        if (DataBrowser.sort) q += '&sort=' + encodeURIComponent(DataBrowser.sort);
        for (var k in DataBrowser.filter) {
            var v = DataBrowser.filter[k];
            if (v !== '' && v !== null && v !== undefined) q += '&filter[' + encodeURIComponent(k) + ']=' + encodeURIComponent(v);
        }
        return q;
    }
    function cellHtml(v, kind) {
        if (v === null || v === undefined) return '<span class="muted">-</span>';
        if (kind === 'bool') return v ? '<span class="badge ok">true</span>' : '<span class="badge">false</span>';
        if (kind === 'json') return '<code class="jsoncell">' + esc(String(v).slice(0, 120)) + '</code>';
        return esc(String(v));
    }
    function renderTableHtml(cols, rows, opts) {
        opts = opts || {};
        var html = '<table><thead><tr>';
        cols.forEach(function (c) {
            var arrow = (opts.sort === c.name) ? ' ▲' : ((opts.sort === '-' + c.name) ? ' ▼' : '');
            html += c.sortable
                ? '<th class="sortable" data-sort="' + esc(c.name) + '">' + esc(c.label) + arrow + '</th>'
                : '<th>' + esc(c.label) + '</th>';
        });
        html += '</tr></thead><tbody>';
        if (rows.length === 0) html += '<tr><td colspan="' + cols.length + '" class="muted">无数据</td></tr>';
        rows.forEach(function (row) {
            html += '<tr>';
            cols.forEach(function (c) {
                var v = row[c.name];
                html += '<td>' + (opts.cellRenderer ? opts.cellRenderer(c, v) : cellHtml(v, c.kind)) + '</td>';
            });
            html += '</tr>';
        });
        html += '</tbody></table>';
        return html;
    }
    function renderPager(id, total, page, size) {
        var pages = Math.max(1, Math.ceil(total / size));
        document.getElementById(id).innerHTML =
            '<span>共 ' + total + ' 条</span>'
            + '<button class="pg" ' + (page <= 1 ? 'disabled' : '') + ' onclick="pageGo(' + (page - 1) + ')">上一页</button>'
            + '<span>第 ' + page + ' / ' + pages + ' 页</span>'
            + '<button class="pg" ' + (page >= pages ? 'disabled' : '') + ' onclick="pageGo(' + (page + 1) + ')">下一页</button>';
    }
    function pageGo(p) {
        DataBrowser.page = p;
        if (DataBrowser.mode === 'task') loadTasks(); else loadDataBrowser();
    }
    function sortBy(col) {
        DataBrowser.sort = (DataBrowser.sort === col) ? ('-' + col) : col;
        DataBrowser.page = 1;
        if (DataBrowser.mode === 'task') loadTasks(); else loadDataBrowser();
    }
    async function loadDataModels() {
        var resp = await apiFetch('/api/data/models');
        if (!resp) return;
        var data = await resp.json();
        var sel = document.getElementById('data-model-select');
        sel.innerHTML = '';
        (data.models || []).forEach(function (m) {
            var o = document.createElement('option');
            o.value = m.key; o.textContent = m.label + '（' + m.table + '）';
            sel.appendChild(o);
        });
        if (sel.options.length) {
            DataBrowser.mode = 'data';
            DataBrowser.model = sel.value;
            loadDataBrowser();
        }
    }
    function onDataModelChange() {
        DataBrowser.mode = 'data';
        DataBrowser.model = document.getElementById('data-model-select').value;
        DataBrowser.page = 1; DataBrowser.sort = ''; DataBrowser.filter = {};
        document.getElementById('data-search').value = '';
        loadDataBrowser();
    }
    async function loadDataBrowser() {
        DataBrowser.mode = 'data';
        DataBrowser.search = document.getElementById('data-search').value;
        DataBrowser.filter = {};
        document.querySelectorAll('#data-filters input[data-col]').forEach(function (inp) {
            if (inp.value.trim() !== '') DataBrowser.filter[inp.getAttribute('data-col')] = inp.value.trim();
        });
        var resp = await apiFetch(dataQ());
        if (!resp) return;
        var data = await resp.json();
        var cols = data.columns || [];
        renderDataFilters(cols);
        document.getElementById('data-table').innerHTML = renderTableHtml(cols, data.rows, { sort: DataBrowser.sort });
        renderPager('data-pager', data.total, data.page, data.size);
    }
    function renderDataFilters(cols) {
        var el = document.getElementById('data-filters');
        var fs = cols.filter(function (c) { return c.filterable; });
        if (fs.length === 0) { el.innerHTML = ''; return; }
        el.innerHTML = fs.map(function (c) {
            return '<div class="f"><span>' + esc(c.label) + '</span>'
                + '<input data-col="' + esc(c.name) + '" placeholder="筛选"></div>';
        }).join('');
    }
    var TASK_STATUS = { 0: ['待处理', 'queued'], 1: ['处理中', 'running'], 2: ['成功', 'done'], 3: ['失败', 'failed'] };
    function taskCell(c, v) {
        if (c.name === 'status') {
            var m = TASK_STATUS[String(v)] || [String(v), ''];
            return '<span class="badge ' + (m[1] === 'done' ? 'ok' : '') + '">' + m[0] + '</span>';
        }
        if (c.name === 'error') {
            if (v === null || v === undefined || v === '') return '<span class="muted">-</span>';
            return '<details><summary>查看错误</summary><pre class="errtext">' + esc(String(v)) + '</pre></details>';
        }
        return cellHtml(v, c.kind);
    }
    async function loadTasks() {
        DataBrowser.mode = 'task';
        DataBrowser.model = 'task';
        if (!DataBrowser.sort) DataBrowser.sort = '-updated_at';
        DataBrowser.filter = {};
        var stage = document.getElementById('task-stage').value;
        var status = document.getElementById('task-status').value;
        if (stage) DataBrowser.filter['stage'] = stage;
        if (status !== '') DataBrowser.filter['status'] = status;
        DataBrowser.search = document.getElementById('task-search').value;
        var resp = await apiFetch(dataQ());
        if (!resp) return;
        var data = await resp.json();
        var cols = data.columns || [];
        document.getElementById('task-table').innerHTML = renderTableHtml(cols, data.rows, { sort: DataBrowser.sort, cellRenderer: taskCell });
        renderPager('task-pager', data.total, data.page, data.size);
    }

    /* ============ 启动 ============ */
    document.getElementById('key').addEventListener('keydown', function (e) { if (e.key === 'Enter') submitKey(); });
    document.getElementById('data-table').addEventListener('click', function (e) {
        var th = e.target.closest('th[data-sort]');
        if (!th) return;
        DataBrowser.mode = 'data';
        sortBy(th.getAttribute('data-sort'));
    });
    document.getElementById('task-table').addEventListener('click', function (e) {
        var th = e.target.closest('th[data-sort]');
        if (!th) return;
        DataBrowser.mode = 'task';
        sortBy(th.getAttribute('data-sort'));
    });

    fetchData();
    setInterval(fetchData, 3000);
