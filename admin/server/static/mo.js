/**
 * Papa Monitor — 前台逻辑
 * 由 mo.js 重构而来：保留主题切换，加入监控后台的取数/渲染/设置逻辑；
 * 表格页由 oao 组件（github.com/ydtg1993/oao）渲染，本文件只负责菜单与装配。
 * 顶层函数/变量需保持全局，供 template.html 的内联 onclick 调用。
 */
'use strict';

/* ============ 主题切换 ============ */
    var themeKey = 'papa-theme';
    function getTheme() { return document.documentElement.getAttribute('data-theme') === 'dark' ? 'dark' : 'light'; }
    function setTheme(t, persist) {
        t = t === 'dark' ? 'dark' : 'light';
        document.documentElement.setAttribute('data-theme', t);
        if (persist !== false) {
            if (typeof MonitorAppearance !== 'undefined') MonitorAppearance.persistTheme(t);
            else { try { localStorage.setItem(themeKey, t); } catch (_) { /* Display remains usable without storage. */ } }
        }
        var btn = document.getElementById('theme-toggle');
        if (btn) btn.textContent = t === 'dark' ? '☀ 浅色' : '☾ 深色';
        if (typeof MonitorAppearance !== 'undefined') MonitorAppearance.refresh();
    }
    setTheme(getTheme(), false);
    var themeBtn = document.getElementById('theme-toggle');
    if (themeBtn) themeBtn.addEventListener('click', function () {
        setTheme(getTheme() === 'dark' ? 'light' : 'dark');
    });

    /* ============ 基础状态 ============ */
    var KEY_STORAGE = 'papa_monitor_key';
    var authGeneration = 0;
    var MODULE_TITLES = { dashboard: '运行概览', queues: '队列治理', tables: '数据', pages: '自定义页', tokens: '访问令牌', obs: '运行报告', logs: '运行日志', settings: '系统设置', section: '数据与资源' };
    var currentModule = 'dashboard';

    function loadKey() {
        try { return localStorage.getItem(KEY_STORAGE) || ''; }
        catch (_) { return ''; } // Unavailable storage never supplies a credential.
    }
    var credentialSeen = loadKey(), tableViewEpoch = 0, monitorViewEpoch = 0;
    function captureMonitorView() {
        var view = monitorViewEpoch, identity = authGeneration, key = loadKey(), module = currentModule;
        return function () { return view === monitorViewEpoch && identity === authGeneration && key === loadKey() && module === currentModule; };
    }
    var legacyTableAdapter = false;
    var LEGACY_TABLE_NOTICE = '当前表格组件版本较旧，仅支持查询；升级 oao 后可使用表格操作';
    function installLegacyTableAdapter() {
        if (legacyTableAdapter || typeof Oao.destroy === 'function') return;
        legacyTableAdapter = true;
        // Old oao has no action cancellation API. Give it a private, read-only
        // transport instead of relying on hidden buttons or detached DOM nodes.
        var prefix = '/api/papa-oao-readonly', nativeFetch = window.fetch;
        var origin = new URL(window.location.href).origin;
        window.fetch = function (input, init) {
            var raw = typeof input === 'string' ? input : input && input.url;
            var url;
            try { url = new URL(raw, window.location.href); } catch (_) { return nativeFetch.apply(this, arguments); }
            if (url.origin !== origin || url.pathname.indexOf(prefix + '/') !== 0) return nativeFetch.apply(this, arguments);
            var suffix = url.pathname.slice(prefix.length), method = String(init && init.method || input && input.method || 'GET').toUpperCase();
            // Only table metadata and a single table's list route are allowed.
            // In particular no action, encoded separator, or subresource reaches the server.
            if (method !== 'GET' || !/^\/[a-zA-Z0-9_-]+$/.test(suffix)) {
                return Promise.resolve(new Response(LEGACY_TABLE_NOTICE, { status: 403, headers: { 'Content-Type': 'text/plain; charset=utf-8' } }));
            }
            var receiver = this;
            return (async function () {
                var epoch = authGeneration, key = loadKey();
                var current = function () { return epoch === authGeneration && key === loadKey(); };
                url.pathname = '/api/oao' + suffix;
                var request = typeof Request !== 'undefined' && input instanceof Request ? new Request(url.href, input) : url.href;
                var resp = await nativeFetch.call(receiver, request, init);
                // A stale 401 must not invoke the old library's unauthorized hook.
                if (!current()) return new Response('会话已变化，请重新查询', { status: 409 });
                if (!resp.ok) return resp;
                var data = await resp.json();
                if (!current()) return new Response('会话已变化，请重新查询', { status: 409 });
                if (data && Array.isArray(data.tables)) data.tables.forEach(function (table) { if (table) table.actions = []; });
                if (data && typeof data === 'object') data.actions = [];
                var headers = new Headers(resp.headers);
                headers.delete('Content-Length'); headers.delete('Content-Encoding');
                return new Response(JSON.stringify(data), { status: resp.status, statusText: resp.statusText, headers: headers });
            })();
        };
        Oao.init({ base: prefix });
        var view = document.getElementById('oao-view');
        if (view && view.parentNode) {
            var notice = document.createElement('p');
            notice.id = 'legacy-table-notice'; notice.className = 'hint'; notice.setAttribute('role', 'status');
            notice.textContent = LEGACY_TABLE_NOTICE;
            view.parentNode.insertBefore(notice, view);
        }
    }
    function retireTableView() {
        tableViewEpoch++;
        var el = document.getElementById('oao-view');
        if (!el) return;
        if (typeof Oao !== 'undefined' && typeof Oao.destroy === 'function') Oao.destroy(el);
        else {
            // Escape uses the legacy component's own cancellation handlers:
            // pending confirmations settle and body-mounted selects close.
            if (document.querySelector && document.querySelector('.oao-mask.open, .oao-select-content:not([hidden])')) {
                document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
            }
            // Isolate late read responses; all legacy writes are blocked above.
            if (el.parentNode && el.cloneNode) el.parentNode.replaceChild(el.cloneNode(false), el);
            else el.innerHTML = '';
        }
    }
    function resetMonitorSessionState() {
        authGeneration++; monitorViewEpoch++; stopPolling(); lastData = null; currentSite = null;
        if (typeof Dialog !== 'undefined' && typeof Dialog.reset === 'function') Dialog.reset();
        if (typeof invalidateLogs === 'function') invalidateLogs();
        if (typeof resetMonitorNavigation === 'function') resetMonitorNavigation();
        if (typeof resetMonitorObservations === 'function') resetMonitorObservations();
        if (typeof resetQueueGovernance === 'function') resetQueueGovernance();
        retireTableView();
        if (typeof Papa !== 'undefined') Papa.leave(true);
        if (typeof workbenchConnection !== 'undefined') workbenchConnection = { updatedAt: null, failed: false, message: '' };
        if (typeof workbenchHTML !== 'undefined') workbenchHTML = new WeakMap();
        if (typeof workbenchKPIValues !== 'undefined') workbenchKPIValues = [];
        ['workbench-kpis','workbench-issues','workbench-sites','dash-queue','dash-sys','dash-dirs','dash-site-ctl','site-tabs','site-meta'].forEach(function (id) {
            var el = document.getElementById(id); if (el) el.innerHTML = '';
        });
        var summaryDefaults = { 'workbench-scope': '全部站点', 'workbench-coverage': '等待快照', 'workbench-issue-count': '—' };
        Object.keys(summaryDefaults).forEach(function (id) {
            var el = document.getElementById(id); if (el) el.textContent = summaryDefaults[id];
        });
        var freshness = document.getElementById('workbench-freshness');
        if (freshness) { freshness.textContent = '尚未获得监控快照'; freshness.classList.remove('is-stale'); }
        if (typeof bellSeen !== 'undefined') bellSeen = null;
        var bell = document.getElementById('bell'), badge = document.getElementById('bell-badge'), panel = document.getElementById('bell-panel');
        if (bell) { bell.setAttribute('aria-expanded', 'false'); bell.classList.remove('pulse'); }
        if (badge) { badge.textContent = '0'; badge.hidden = true; badge.className = 'bell-badge'; }
        if (panel) { panel.innerHTML = ''; panel.hidden = true; }
        ['token-table','wl-file-info','wl-msg','shutdown-msg','obs-dir','obs-runs','obs-msg','obs-tabs','obs-meta','obs-content'].forEach(function (id) {
            var el = document.getElementById(id); if (el) el.textContent = '';
        });
        var whitelist = document.getElementById('wl-input'); if (whitelist) { whitelist.value = ''; whitelist.textContent = ''; }
        var viewer = document.getElementById('obs-viewer-sec'); if (viewer) viewer.hidden = true;
        if (typeof obsState !== 'undefined') obsState = { dir: '', runs: [], run: '', file: '', requestSeq: obsState.requestSeq + 1 };
    }
    function saveKey(k) { resetMonitorSessionState(); localStorage.setItem(KEY_STORAGE, k); credentialSeen = k; }
    function clearKey() { resetMonitorSessionState(); localStorage.removeItem(KEY_STORAGE); credentialSeen = ''; }
    function syncMonitorCredential() {
        var key = loadKey();
        if (key === credentialSeen) return false;
        resetMonitorSessionState(); credentialSeen = key; pendingLoginRefresh = true;
        if (key || !authRequired()) startPolling();
        else { setStatus(false, '需要密钥'); showLogin(false); }
        return true;
    }
    window.addEventListener('storage', function (event) {
        if ((event.key !== KEY_STORAGE && event.key !== null) || event.storageArea !== localStorage) return;
        syncMonitorCredential();
    });
    // 服务端默认要求登录；自定义身份提供方可调整登录遮罩的展示。
    // 此标记只影响页面展示，不改变服务端对每个管理请求的认证。
    function authRequired() { return document.documentElement.getAttribute('data-auth-required') === '1'; }

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
    var pendingLoginRefresh = false;
    function submitKey() { saveKey(document.getElementById('key').value); pendingLoginRefresh = true; startPolling(); }
    function refreshAfterLogin() {
        if (!pendingLoginRefresh) return;
        pendingLoginRefresh = false;
        if (currentModule === 'tables' && currentTable) openTable(currentTable, document.getElementById('moduleTitle').textContent);
        if (currentModule === 'settings') { Papa.leave(true); Papa.renderSettings(document.getElementById('custom-settings')); fetchSettings(); }
        if (currentModule === 'pages' && currentPage) openPage(currentPage.key, currentPage.label);
        if (currentModule === 'logs') fetchLogs();
        if (currentModule === 'tokens') fetchTokens();
        if (currentModule === 'obs') fetchObsReports();
    }
    function setStatus(ok, text) {
        document.getElementById('dot').className = ok ? 'dot ok' : 'dot';
        document.getElementById('statusText').textContent = text;
        if (typeof setWorkbenchConnection === 'function') setWorkbenchConnection(ok, text);
        if (typeof setQueueGovernanceConnection === 'function') setQueueGovernanceConnection(ok, text);
        if (!ok && typeof setMonitorVisualConnection === 'function') setMonitorVisualConnection(false);
    }

    function switchModule(name, view) {
        var target = document.getElementById('mod-' + name);
        if (!target) return false;
        if (typeof Papa !== 'undefined' && !Papa.leave(false)) return false;
        monitorViewEpoch++;
        if (typeof Dialog !== 'undefined' && typeof Dialog.reset === 'function') Dialog.reset();
        if (typeof destroyMonitorVisuals === 'function') destroyMonitorVisuals();
        if (currentModule === 'tables') retireTableView();
        setNavigationOpen(false);
        if (typeof invalidateLogs === 'function') invalidateLogs();
        currentModule = name;
        if (view && view.table) currentTable = view.table;
        if (view && view.page) currentPage = { key: view.page, label: view.label };
        if (view && view.section && typeof currentSection !== 'undefined') currentSection = view.section;
        document.querySelectorAll('.module').forEach(function (m) { m.classList.remove('active'); });
        document.getElementById('mod-' + name).classList.add('active');
        document.querySelectorAll('.nav-item').forEach(function (b) {
            b.classList.toggle('active', b.dataset.module === name);
        });
        document.getElementById('moduleTitle').textContent = view && view.label || MODULE_TITLES[name];
        if (typeof Oao !== 'undefined' && Oao.motion) Oao.motion.animate(document.getElementById('mod-' + name), 'enter');
        if (name === 'dashboard' && lastData && typeof renderMonitorVisuals === 'function') renderMonitorVisuals(lastData);
        if (name === 'settings') { Papa.renderSettings(document.getElementById('custom-settings')); fetchSettings(); }
        if (name === 'logs') { fetchLogs(); }
        if (name === 'tokens') { fetchTokens(); }
        if (name === 'obs') { fetchObsReports(); }
        if (name === 'queues') {
            var queueMessage = document.getElementById('queue-gov-msg');
            if (queueMessage) queueMessage.textContent = '';
            if (lastData) renderQueues(lastData.queues);
        }
        if (typeof syncMonitorNavigation === 'function') syncMonitorNavigation(true);
        return true;
    }

    function setNavigationOpen(open) {
        var nav = document.getElementById('main-nav'), toggle = document.getElementById('nav-toggle');
        if (nav) nav.classList.toggle('nav-open', open);
        if (toggle) toggle.setAttribute('aria-expanded', String(open));
        if (open && nav) { var active = nav.querySelector('.nav-category.active') || nav.querySelector('.nav-category'); if (active) active.focus(); }
    }
    function toggleNavigation() {
        setNavigationOpen(document.getElementById('nav-toggle').getAttribute('aria-expanded') !== 'true');
    }

    /* ============ 表格（oao 组件） ============ */
    var currentTable = null; // 当前打开的表 key（切站点 tab 时要按新站点重画它）

    /**
     * tableFilterOpts 让表格跟着**当前站点 tab** 走：切到某站再打开任务表，就带着 `site=<该站>` 的
     * 预置筛选打开（oao 会把它填进筛选栏，用户能改能清）。概览不预设 —— 它本来就是看全部。
     */
    function tableFilterOpts() {
        if (currentSite === null || (typeof monitorTableSupportsSite === 'function' && !monitorTableSupportsSite(currentTable))) return { filter: {} };
        return { filter: { site: currentSite } };
    }

    /** 打开某张表：切到表格模块、点亮对应菜单项，再让 oao 渲染 */
    async function openTable(key, label, btn, initialFilter) {
        if (!switchModule('tables', { table: key, label: label || '数据' })) return false;
        await renderTableView(key, initialFilter);
        return true;
    }

    async function renderTableView(key, initialFilter) {
        // 旧 oao 没有销毁/请求代次保护，隔离旧节点使其晚到渲染无法写入新视图。
        if (typeof Oao.destroy !== 'function') retireTableView();
        var view = document.getElementById('oao-view'), epoch = ++tableViewEpoch, identity = authGeneration;
        var credential = loadKey();
        var current = function () { return epoch === tableViewEpoch && identity === authGeneration && credential === loadKey() && currentModule === 'tables' && currentTable === key && view === document.getElementById('oao-view'); };
        try {
            // A server-owned marker distinguishes catalog tables from explicitly registered business tables.
            var entry = typeof monitorNavigation !== 'undefined' && monitorNavigation.tables.find(function (item) { return item.key === key; });
            if (entry && entry.frameworkData) {
                if (typeof Oao.destroy === 'function') Oao.destroy(view);
                view.textContent = '正在检查框架表…';
                var response = await apiFetch('/api/framework-tables/' + encodeURIComponent(key), { current: current });
                if (!current()) return;
                if (!response || !response.ok) { view.textContent = '框架表状态读取失败，请重新打开此表重试。'; return; }
                var status = await response.json();
                if (!current()) return;
                if (status.state !== 'available') { view.textContent = status.message || '框架表暂不可用，请重新打开此表重试。'; return; }
            }
            // 第三个参数 = oao 的预置筛选（需要 oao ≥ 带 opts.filter 的那版；老版本会忽略它）
            await Oao.render(view, key, initialFilter ? { filter: Object.assign({}, tableFilterOpts().filter, initialFilter) } : tableFilterOpts());
        } catch (e) {
            if (current()) {
                view.innerHTML = '<div class="empty">表格加载失败：' + esc(e && e.message || '请求失败') + '</div>';
            }
        }
    }

    /** 按 Group 分组渲染当前分类的动态菜单；固定分类不依赖 oao 的挂载节点。 */
    var navRequestSeq = 0;
    async function renderNav() {
        var seq = ++navRequestSeq, epoch = authGeneration, key = loadKey();
        var current = function () { return seq === navRequestSeq && epoch === authGeneration && key === loadKey(); };
        var tables = [];
        try { tables = (await Oao.list(true)) || []; } catch (e) { tables = []; }
        if (!current()) return;
        var pages = [], resp = await apiFetch('/api/pages', { current: current });
        if (resp) {
            try { pages = (await resp.json()).pages || []; } catch (e) { pages = []; }
        }
        if (!current()) return;
        if (typeof setMonitorNavigation === 'function') setMonitorNavigation(tables, pages);
    }

    /* ============ 自定义页（业务用 app.UsePage 注册） ============ */
    /**
     * 页脚本用它注册渲染函数 —— app.UsePage 的 Script 里写：
     *   Papa.page('review', function (el, meta) { el.innerHTML = '…'; });
     * el 是 #page-view；Toast / Dialog / skeletonRows / esc / apiFetch / apiPost 都是全局可用的。
     */
    // Papa.page/settings and lifecycle handling are defined by extensions.js.
    var currentPage = null;

    /** 打开某个自定义页：切到 pages 模块、点亮菜单项，再交给页脚本渲染 */
    function openPage(key, label, btn) {
        if (!switchModule('pages', { page: key, label: label || '自定义页' })) return false;
        var view = document.getElementById('page-view');
        view.innerHTML = '';
        if (!Papa.render(key, view, { key: key, label: label })) {
            view.innerHTML = '<div class="empty">页面 ' + esc(key)
                + ' 没有注册渲染函数：检查 app.UsePage 的 Script 里是否调了 Papa.page("'
                + esc(key) + '", fn)</div>';
        }
        return true;
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
        var returnFocus = null;

        function ensure() {
            if (mask) return;
            mask = document.createElement('div');
            mask.className = 'dlg-mask';
            mask.innerHTML = '<div class="dlg" role="dialog" aria-modal="true" aria-labelledby="papa-dialog-title" tabindex="-1">'
                + '<h3 id="papa-dialog-title"></h3><div class="dlg-body"></div><div class="dlg-actions"></div></div>';
            mask.addEventListener('mousedown', function (e) { if (e.target === mask) close(null); });
            document.body.appendChild(mask);
            elTitle = mask.querySelector('h3');
            elBody = mask.querySelector('.dlg-body');
            elActions = mask.querySelector('.dlg-actions');
            document.addEventListener('keydown', function (e) {
                if (!mask.classList.contains('open')) return;
                if (e.key === 'Escape') { e.preventDefault(); close(null); }
                if (e.key === 'Tab') {
                    var targets = Array.from(mask.querySelectorAll('button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')).filter(function (el) { return el.offsetParent !== null; });
                    var first = targets[0], last = targets[targets.length - 1];
                    if (!first) { e.preventDefault(); mask.querySelector('.dlg').focus(); }
                    else if (!mask.contains(document.activeElement) || (e.shiftKey && document.activeElement === first) || (!e.shiftKey && document.activeElement === last)) {
                        e.preventDefault(); (e.shiftKey ? last : first).focus();
                    }
                }
            });
        }

        function close(value) {
            if (!mask || !mask.classList.contains('open')) return;
            mask.classList.remove('open');
            var done = settle;
            settle = null;
            if (returnFocus && returnFocus.isConnected) returnFocus.focus();
            // The opener may have been replaced by a render. A hidden dialog
            // must not retain focus while the caller restores its new button.
            if (mask.contains(document.activeElement)) document.activeElement.blur();
            returnFocus = null;
            if (done) done(value);
        }

        function open(opts) {
            ensure();
            // A late Trace can replace a pending confirmation. Always settle the
            // displaced dialog so callers release their operation locks.
            close(null);
            returnFocus = document.activeElement;
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
            close: function () { close(null); },
            reset: function () {
                close(null);
                if (!mask) return;
                elTitle.textContent = ''; elBody.innerHTML = ''; elActions.innerHTML = '';
            }
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
    function bar(cls, p) { return '<div class="bar ' + cls + '"><span style="width:' + p + '%"></span></div>'; }

    /* ============ 渲染 ============ */
    function renderSystem(s, generatedAt) {
        var el = document.getElementById('dash-sys');
        if (!s) { el.innerHTML = '<div class="empty">无系统指标</div>'; return; }
        el.innerHTML = ['cpu','mem','disk'].map(function (key) {
            var v = MonitorObservations.resource(s,key,MonitorObservations.time(generatedAt));
            var label = {cpu:'CPU',mem:'内存',disk:'磁盘'}[key], valid = v.value !== null;
            return '<div class="card"><div class="label">' + label + (key === 'disk' ? ' · 根文件系统' : ' · 系统采样') + '</div><div class="value">' + (valid ? v.value.toFixed(1) + '%' : '—') + '</div>'
                + '<div class="sub">' + (valid && key!=='cpu' ? fmtBytes(s[key+'_used']) + ' / ' + fmtBytes(s[key+'_total']) : v.state) + '</div>'
                + (valid ? bar(key,pct(v.value)) : '') + '</div>';
        }).join('');
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
            + '<div class="tile queued"><div class="k">排队</div><div class="v">' + workbenchNumber(workbenchCount(q.queue_len)) + '</div></div>'
            + '<div class="tile running"><div class="k">未完成（含排队）</div><div class="v">' + workbenchNumber(workbenchCount(q.in_progress)) + '</div></div>'
            + '<div class="tile done"><div class="k">完成</div><div class="v">' + workbenchNumber(workbenchCount(q.completed)) + '</div></div>'
            + '<div class="tile failed"><div class="k">错误</div><div class="v">' + workbenchNumber(workbenchCount(q.failed)) + '</div></div>'
            + '<div class="tile submitted"><div class="k">已提交</div><div class="v">' + workbenchNumber(workbenchCount(q.submitted)) + '</div></div>'
            + '</div>';
    }
    // 概览里每个阶段压缩成一行：阶段名 + 三个关键数字 + 完成小字。
    // 三个数字是「现在怎么样」（排队/未完成（含排队）/错误），完成量是累计视角，放小字不占视线；
    // 已提交=累计提交（≈完成+失败+在途），在概览里重复感太强，只在站点 Tab 的完整详情里出现。
    function stageLine(name, st) {
        var q = (st && st.queue) || {};
        function mini(cls, label, val) {
            return '<span class="mini ' + cls + '" title="' + label + '">' + '<i></i>' + label
                + ' <b>' + workbenchNumber(workbenchCount(val)) + '</b></span>';
        }
        return '<div class="stage-line">'
            + '<span class="stage-name">' + esc(stageLabel(name, st)) + '</span>'
            + '<span class="mini-group">'
            + mini('queued', '排队', q.queue_len)
            + mini('running', '未完成（含排队）', q.in_progress)
            + mini('failed', '错误', q.failed)
            + '</span>'
            + '<span class="stage-done">完成 ' + workbenchNumber(workbenchCount(q.completed)) + '</span>'
            + '</div>';
    }
    // 站点 Tab 里的完整阶段详情：五格大数字 + 累计耗时 + Worker 明细表。
    // 这块原来是独立的「任务队列」模块，模块已并入 Dashboard 的「任务队列概览」——
    // 概览（全部）保持紧凑的站点卡片，切到某站点时才展开到这一层，信息不丢。
    function stageGlobal(st) {
        var g = (st && st.global) || {};
        return '累计任务 ' + (g.TotalTasks || 0) + ' · 累计失败 ' + (g.TotalFailed || 0)
            + ' · 平均 ' + fmtDuration(g.AvgTime) + ' · 最大 ' + fmtDuration(g.MaxTime)
            + ' · 最小 ' + fmtDuration(g.MinTime);
    }
    // workerTable 画某阶段的 Worker 明细（池内每个 worker 的任务数/失败数/耗时快照）。
    function workerTable(st) {
        var workers = (st && st.workers) || {};
        var ids = Object.keys(workers);
        if (ids.length === 0) return '<div class="muted">暂无 Worker 明细</div>';
        var rows = ids.map(function (id) {
            var w = workers[id];
            return '<tr><td>#' + esc(id) + '</td><td>' + (w.TotalTasks || 0) + '</td><td>' + (w.FailedTasks || 0)
                + '</td><td>' + fmtDuration(w.TotalTime) + '</td><td>' + fmtDuration(w.MaxTime)
                + '</td><td>' + fmtDuration(w.MinTime) + '</td></tr>';
        }).join('');
        return '<div class="table-wrap"><table><thead><tr><th>Worker</th><th>任务</th><th>失败</th>'
            + '<th>总耗时</th><th>最大</th><th>最小</th></tr></thead><tbody>' + rows + '</tbody></table></div>';
    }
    function stageDetail(name, st) {
        return '<div class="stage"><h3>' + esc(stageLabel(name, st)) + '</h3>' + stageTiles(st.queue)
            + '<div class="muted">' + stageGlobal(st) + '</div>'
            + '<div class="stage-workers"><div class="muted">Worker 明细</div>' + workerTable(st) + '</div></div>';
    }
    // 任务队列概览分两种形态：
    //   - 概览（全部站点）：**按站点分卡片**。多站都叫 catalog/detail，平铺在一起只看阶段名
    //     分不清是哪个站；卡片里每个阶段只占一行，扫一眼就知道哪站在排队/出错。
    //   - 选中某站点 Tab：显示该站的多阶段详情（五格大数字 + 累计耗时 + Worker 明细表）。
    function renderQueueSummary(stages, sites) {
        var el = document.getElementById('dash-queue');
        var names = Object.keys(stages || {});
        if (currentSite === null && sites && sites.length > 0) {
            el.classList.add('queue-site-grid');
            var bySite = {};
            names.forEach(function (name) {
                var st = stages[name];
                // 正常服务端一定带 site（阶段键只是 `site:stage` 的复合标识）；老快照没有 site 时
                // 从键里取站点段兜底，至少不会把一条阶段错归到别的站。
                var site = (st && st.site) || name.split(':')[0];
                (bySite[site] = bySite[site] || []).push(name);
            });
            el.innerHTML = sites.map(function (site) {
                var list = (bySite[site] || []).slice().sort();
                var head = '<div class="site-card-head"><h3>' + esc(siteLabel(site)) + '</h3>'
                    + '<span class="muted">' + list.length + ' 个阶段</span></div>';
                if (list.length === 0) {
                    return '<div class="site-card">' + head + '<div class="empty">暂无活跃阶段</div></div>';
                }
                return '<div class="site-card">' + head
                    + '<div class="stage-lines">' + list.map(function (name) {
                        return stageLine(name, stages[name]);
                    }).join('') + '</div></div>';
            }).join('');
            return;
        }
        el.classList.remove('queue-site-grid');
        if (names.length === 0) { el.innerHTML = '<div class="empty">暂无活跃阶段</div>'; return; }
        el.innerHTML = names.map(function (name) {
            return stageDetail(name, stages[name]);
        }).join('');
    }
    /* ---- 治理队列（错误队列 + 轮询队列，各一条全局） ---- */
    var QUEUE_KINDS = {
        error_queue: { label: '错误队列', desc: '失败任务重投', path: '/api/errorqueue/process', key: 'processed', done: '已重新投递' },
        repeat_queue: { label: '轮询队列', desc: '周期任务重投', path: '/api/repeatqueue/process', key: 'repolled', done: '已重投' }
    };
    function queueMeta(name) {
        var kind = QUEUE_KINDS[name];
        if (!kind) return null;
        return { label: kind.label, desc: kind.desc, url: kind.path, key: kind.key, done: kind.done };
    }
    // 面板上的顺序：错误队列在前、轮询队列在后（按名字字典序）
    function queueKeys(data) {
        return Object.keys(data || {}).filter(function (n) { return QUEUE_KINDS[n] !== undefined; }).sort();
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
        if (typeof q.enabled !== 'boolean' || typeof q.running !== 'boolean') return '<span class="badge off">状态未知</span>';
        if (!q.enabled) return '<span class="badge off"' + title + '>自动轮询已停用</span>';
        return '<span class="badge ok"' + title + '>空闲</span>';
    }
    function renderQueues(queues) {
        var el = document.getElementById('queue-gov');
        var data = queues || {};
        if (!queueKeys(data).length) { el.innerHTML = '<div class="empty">宿主尚未提供后台治理队列快照。</div>'; return; }
        var focusedQueue = el.contains(document.activeElement) && document.activeElement.dataset ? document.activeElement.dataset.queue : null;
        el.innerHTML = '<table class="qtable"><thead><tr>'
            + '<th>队列</th><th>状态</th><th>上次执行</th><th>处理量</th><th>待处理</th><th>操作</th>'
            + '</tr></thead><tbody>'
            + queueKeys(data).map(function (name) {
                var m = queueMeta(name);
                var q = data[name] || {};
                var ran = q.runs > 0;
                var backlog = workbenchTime(q.backlog_at) ? workbenchCount(q.backlog) : null;
                return '<tr>'
                    + '<td><b>' + esc(m.label) + '</b><div class="muted">' + esc(name) + ' · ' + esc(m.desc) + '</div></td>'
                    + '<td>' + queueStatus(q) + (q.last_error ? '<div class="hint queue-error">上次错误：' + esc(q.last_error) + '</div>' : '') + '</td>'
                    + '<td title="完成于 ' + esc(absTime(q.last_finish_at)) + '">' + esc(fmtAgo(q.last_finish_at))
                    + '<div class="muted">' + (ran ? ('耗时 ' + fmtDuration(q.last_duration)) : '尚未执行') + '</div></td>'
                    + '<td>' + workbenchNumber(workbenchCount(q.running ? q.run_processed : q.last_processed))
                    + '<div class="muted">' + (q.running ? '本轮已处理' : '上次处理') + ' · 累计 ' + workbenchNumber(workbenchCount(q.total_processed)) + ' · 执行 ' + workbenchNumber(workbenchCount(q.runs)) + ' 轮</div></td>'
                    + '<td>' + (backlog === null ? '—（未知）' : backlog > 0 ? '<span class="badge warn">' + backlog + '</span>' : '0')
                    + '<div class="muted">' + esc(fmtAgo(q.backlog_at)) + '采样</div></td>'
                    // 队列名是运行期才知道的，所以走 data-* + 从 dataset 取 ——
                    // 与熔断那个「恢复」按钮同一套写法：内联属性里只有固定代码，运行期值不拼进 JS 字符串
                    + '<td><button class="btn" style="margin-top:0" data-queue="' + esc(name)
                    + '" onclick="triggerQueue(this.dataset.queue)"' + (queueFlights[name] ? ' disabled' : '') + '>立即执行</button></td>'
                    + '</tr>';
            }).join('')
            + '</tbody></table>';
        if (focusedQueue) el.querySelectorAll('[data-queue]').forEach(function (b) { if (b.dataset.queue === focusedQueue && !b.disabled) b.focus(); });
    }
    var queueFlights = Object.create(null);
    async function triggerQueue(name) {
        var m = queueMeta(name);
        if (!m || queueFlights[name] || currentModule !== 'queues') return;
        var current = captureMonitorView(), flight = {};
        queueFlights[name] = flight;
        var el = document.getElementById('queue-gov-msg');
        function buttons(disabled) {
            document.querySelectorAll('[data-queue]').forEach(function (button) { if (button.dataset.queue === name) button.disabled = disabled; });
        }
        buttons(true);
        try {
            var confirmed = await Dialog.confirm({ title: '执行' + m.label,
                body: '<p>将扫描并重新投递符合条件的任务，作用于<b>全部站点</b>，不受当前站点筛选影响。</p><p>此操作可能启动实际采集，不是只刷新统计。</p>', okLabel: '确认全站执行' });
            if (!confirmed || !current()) return;
            el.textContent = m.label + '处理中（全部站点）...';
            var resp = await apiRequest(m.url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}', current: current });
            if (!resp || !resp.ok || !current()) return;
            var data = await resp.json();
            if (!current()) return;
            Toast.ok(m.label + '：' + m.done + ' ' + (data[m.key] || 0) + ' 个任务（全部站点）');
            fetchData();
        } catch (e) {
            if (current()) Toast.err('响应读取失败，操作结果尚未确认，请刷新队列状态');
        } finally {
            if (queueFlights[name] === flight) delete queueFlights[name];
            if (current()) { el.textContent = ''; buttons(false); }
        }
    }
    /* ============ 站点 Tab（多站点时按站点把面板分开） ============ */
    // currentSite：null = 概览（全部）；其余 = 站点键（站点是必填维度，没有"空键"那一档）。
    var currentSite = null;

    // 站点键直接就是显示名（没有"默认 scope"那一档了：声明要有 Key、任务要有 site）。
    function siteLabel(key) { return key; }

    // stageLabel 阶段面板的标题：优先用条目里带的**裸阶段名**（st.stage）—— 快照的键是
    // `site:stage`（阶段名只在本站内唯一，两个站可以各有 catalog），标题里带上站点只会更长；
    // 站点维度由上面的站点 Tab 表达。老服务端不带 stage 字段时回落到键。
    function stageLabel(name, st) {
        return (st && st.stage) || name;
    }

    // siteView 按当前站点过滤阶段；治理队列是全局各一条，不跟站点走。
    // siteList 是**阶段入口卡片**要用的站点清单：它以 sites（声明播种的站点快照）为准，
    // 不是从 entries 推出来的 —— 没有入口阶段的站也该有卡片，否则"暂停/恢复"那个按钮就跟着没了。
    function siteView(stages, queues, sites) {
        var allSites = Object.keys(sites || {}).sort();
        if (currentSite === null) {
            return { stages: stages, queues: queues, sites: allSites };
        }
        var outStages = {};
        Object.keys(stages || {}).forEach(function (name) {
            if ((stages[name].site || '') === currentSite) outStages[name] = stages[name];
        });
        // 切到某站：阶段与站点清单都只留那一站（概览时全列出来，当总控台用）
        return {
            stages: outStages, queues: queues,
            sites: allSites.filter(function (s) { return s === currentSite; }),
        };
    }

    function renderSiteTabs(data) {
        var el = document.getElementById('site-tabs');
        if (!el) return;
        var sites = (data && data.sites) || {};
        var keys = buildWorkbench(data, null).rows.map(function (row) { return row.site; }).sort();
        if (currentSite !== null && keys.indexOf(currentSite) < 0) {
            currentSite = null; // 站点没了回概览
            refreshSiteModule();
        }
        if (keys.length === 0) { // 还没有站点快照（老引擎 / 没播种）：清理失效范围后整排隐藏
            el.innerHTML = '';
            workbenchHTML.delete(el);
            el.hidden = true;
            document.getElementById('site-meta').hidden = true;
            return;
        }
        var focus = el.contains(document.activeElement) && document.activeElement.dataset ? Object.assign({}, document.activeElement.dataset) : null;
        if (focus && focus.site && keys.indexOf(focus.site) < 0) focus = {all:'true'};
        var tabsHTML = ['<button class="site-tab' + (currentSite === null ? ' active' : '') +
            '" data-all="true">概览（全部）</button>'].concat(keys.map(function (k) {
                return '<button class="site-tab' + (currentSite === k ? ' active' : '') +
                    '" data-site="' + esc(k) + '">' + esc(siteLabel(k)) + '</button>';
            })).join('');
        if (workbenchHTML.get(el) !== tabsHTML) { el.innerHTML = tabsHTML; workbenchHTML.set(el, tabsHTML); }
        // 与队列按钮同一套：data-* 带运行期值，内联属性里只有固定代码（站点名不拼进 JS 字符串）
        el.querySelectorAll('.site-tab').forEach(function (b) {
            b.onclick = function () { switchSite(b.dataset.all === 'true' ? null : b.dataset.site); };
            b.setAttribute && b.setAttribute('aria-pressed', String(b.dataset.all === 'true' ? currentSite === null : b.dataset.site === currentSite));
            if (focus && focus.all === b.dataset.all && focus.site === b.dataset.site) b.focus();
        });
        el.hidden = false;
        renderSiteMeta(sites);
        if (typeof syncMonitorSiteScope === 'function') syncMonitorSiteScope();
    }

    function renderSiteMeta(sites) {
        var meta = document.getElementById('site-meta');
        if (!meta) return;
        var stat = currentSite === null || !Object.prototype.hasOwnProperty.call(sites, currentSite) ? null : sites[currentSite];
        if (!stat) { meta.hidden = true; meta.innerHTML = ''; return; }
        var parts = [
            '<b>' + esc(siteLabel(stat.key)) + '</b>',
            esc(stat.base_url || '—'),
            (stat.stage_count || 0) + ' 个阶段',
            // 这两列只由**手动暂停**写（熔断触发不写它），所以别叫"熔断闸住" ——
            // 一次后台点的暂停会被显示成熔断，读的人会去找"哪来的失败"。
            stat.breaker_paused
                ? '<b>已暂停</b>（' + esc(fmtAgo(stat.breaker_paused_at)) + '）'
                : '未暂停'
        ];
        parts.push('<span class="muted">在「数据与资源 → 采集任务」查看本站任务</span>');
        meta.innerHTML = parts.join(' ｜ ');
        meta.hidden = false;
    }

    function switchSite(site) {
        currentSite = site;
        if (lastData) renderAll(lastData); // 手上有快照就直接重画，不必等下一次轮询
        refreshSiteModule();
    }

    function refreshSiteModule() {
        // 正开着某张表时，也按新站点重画它 —— 切站点 tab 就该连表里的筛选一起跟着换
        if (currentModule === 'tables' && currentTable && (typeof monitorTableSupportsSite !== 'function' || monitorTableSupportsSite(currentTable))) {
            renderTableView(currentTable);
        }
    }

    var lastData = null;

    function renderAll(data) {
        lastData = data;
        renderSiteTabs(data);
        renderWorkbench(data);
        if (typeof renderMonitorVisuals === 'function') renderMonitorVisuals(data);
        var view = siteView(data.stages, data.queues, data.sites);
        renderSystem(data.system, data.generated_at);
        renderDirs(data.system && data.system.dirs);
        renderQueueSummary(view.stages, view.sites);
        renderQueues(view.queues);
        if (typeof renderQueueGovernance === 'function') renderQueueGovernance(data, currentSite);
        // 键名是**复数**：服务端 monitor.go 写的是 resp["breakers"]（数组，每站一条）。
        // 这里曾经写成 data.breaker，于是横幅永远拿不到数据、（list || []）把它吞成空数组，
        // 一个字都不报 —— 多站熔断横幅其实一直没显示过。
        // **不过滤**：铃铛是告警，切到哪个站点 tab 都该看得见（切走了反而看不见出事的是哪个站）。
        renderBell(data.breakers);
        // 站点控制卡片：跟着站点 Tab 过滤（概览 = 全部站点），暂停状态取自同一轮的 breakers。
        renderSiteControl(view.sites, data.breakers);
    }

    /* ---- 暂停与告警：右上角铃铛 + 下拉面板（多站时逐 scope 一行） ---- */
    // 为什么不是红色横幅：声明了 `SiteSpec.StartPaused` 的站一加载就停着，横幅会变成常驻的一大块
    //（把正文挤下去），而它平时什么也不必说。铃铛把"有东西停着"压成一个徽标数字：
    //   - 数字 = 被闸住的 scope 数（含声明期的 StartPaused）；
    //   - 颜色 = 语义：里面有熔断触发就是红的，只有手动/声明期就用琥珀 —— 一眼看出"是不是出事了"。
    // 点开才列出细节（站点 / 阶段 / 原因 / 暂停于 + 逐站「恢复」）；再点一次、点面板外部、按 Esc 关闭。
    // 状态跟着 /api/monitor 每轮刷新一起来（不额外发请求），所以别的标签页 / 别的操作人触发的
    // 熔断，这边几秒内也能看到。
    //
    // 面板**不跟站点 Tab 过滤**（告警不该被切走的 tab 藏起来）；跟 tab 走的是仪表盘上那张
    // 「站点控制」卡 —— 那张是控制视角，这张是告警视角。
    var bellSeen = null; // 上一轮见过的熔断 scope；null = 还没画过（首帧不脉冲，免得一进页面就抖）
    function renderBell(list) {
        var btn = document.getElementById('bell');
        var badge = document.getElementById('bell-badge');
        var panel = document.getElementById('bell-panel');
        if (!btn || !badge || !panel) return;
        var paused = (list || []).filter(function (b) { return b && b.paused; });
        var tripped = paused.filter(function (b) { return b.source === 'breaker'; });

        // 徽标：数字 = 被闸住的 scope 数，颜色分语义；没有就整个藏起来（空徽标比没徽标更吵）
        badge.hidden = !paused.length;
        badge.textContent = paused.length;
        badge.className = 'bell-badge' + (paused.length ? (tripped.length ? ' breaker' : ' manual') : '');

        // 脉冲只给**新出现的熔断**：页面开着时出事要看得见；而 StartPaused 那种"一加载就停着"
        // 不该被当成新告警（首帧也不算 —— 否则每次刷新页面都抖一下）。
        var now = tripped.map(scopeKey);
        if (bellSeen !== null && now.some(function (k) { return bellSeen.indexOf(k) < 0; })) pulseBell(btn);
        bellSeen = now;

        var html = paused.map(bellRow).join('');
        if (paused.length > 1) {
            html += '<div class="bell-all"><span class="bell-meta">共 ' + paused.length +
                ' 个 scope 被闸住</span>' +
                '<button class="btn" onclick="resumeCrawling()">全部重试恢复</button></div>';
        }
        if (!html) {
            html = '<div class="bell-meta">当前没有被闸住的站点</div>';
        }
        panel.innerHTML = html; // 每一段都过了 esc()
    }

    // scopeKey 一个 scope 的稳定标识：站点 + 阶段（阶段级暂停的 scope 带 stage，整站的不带）。
    // 用 \u0000 拼是为了两个字段怎么取值都不会拼出同一个键。
    function scopeKey(b) { return (b.site || '') + '\u0000' + (b.stage || ''); }

    // pulseBell 脉冲两下。先把上一次的类摘掉再加回去 —— 中间必须读一次布局属性强制重排，
    // 否则"摘掉"与"加回"会被浏览器合并成一次样式变更，动画不会重新开始（第二次告警就不抖了）。
    function pulseBell(btn) {
        btn.classList.remove('pulse');
        void btn.offsetWidth;
        btn.classList.add('pulse');
    }

    // bellRow 面板里的一行：**两行式**（头部一行 + 一条等宽小字的 meta）。
    // 面板只有五百来像素宽，把"整句散文 + 按钮"塞进一行会折成三四行、按钮悬在中间；
    // 拆开之后第一行永远是"谁 + 结论 + 动作"，细节退到第二行。
    function bellRow(b) {
        var at = b.paused_at ? new Date(b.paused_at) : null;
        var when = (at && timeValid(at)) ? at.toLocaleString() : '';
        // **手动暂停与熔断触发分开说**：一律按熔断文案渲染就会读出"阈值 0 / 窗口内 0 次失败"
        // 这种幻觉（熔断没启用时阈值就是 0），也是一次手动暂停被说成"熔断"。
        //
        // 判据只看 `source`。**别回去用"enabled && stage"猜**：阶段级的手动暂停也带 stage
        //（那是它的 scope），在熔断启用着的站上会被猜成熔断 —— 这正是加 source 字段的原因。
        var tripped = b.source === 'breaker';
        var head = tripped ? '抓取已暂停（熔断）' : ('抓取已暂停（' + esc(b.reason || '手动') + '）');
        // meta 的几段：有哪段写哪段（整站暂停没有阶段；熔断才有窗口与失败数）
        var meta = [];
        if (b.stage) meta.push('阶段 ' + esc(b.stage));
        if (tripped) {
            meta.push(esc(fmtSpan(b.window / 1e9)) + ' 窗口内 ' + (b.failures || 0) +
                ' 次终态失败，阈值 ' + (b.threshold || 0));
        }
        if (when) meta.push('暂停于 ' + esc(when));
        return '<div class="bell-item">' +
            '<div class="bell-head">' +
            '<span class="bell-where">站点 ' + esc(b.site || '') + '</span>' +
            '<span class="bell-head-text">' + head + '</span>' +
            (tripped ? '<button class="btn" data-site="' + esc(b.site || '') + '" onclick="openBreakerAnalysis(this.dataset.site)">分析原因</button>' : '') +
            '<button class="btn" data-scope="' + esc(b.site || '') +
            '" onclick="resumeCrawling(this.dataset.scope)">重试恢复</button>' +
            '</div>' +
            (meta.length ? '<div class="bell-meta">' + meta.join('｜') + '</div>' : '') +
            '</div>';
    }

    // 铃铛的开合：点铃铛切换；点面板外部、按 Esc 关闭（后两个监听在文件末尾注册一次）。
    function toggleBell(ev) {
        if (ev) ev.stopPropagation(); // 不拦住的话，"点面板外部就关"会当场把它关回去
        var panel = document.getElementById('bell-panel');
        var btn = document.getElementById('bell');
        if (!panel || !btn) return;
        panel.hidden = !panel.hidden;
        btn.setAttribute('aria-expanded', panel.hidden ? 'false' : 'true');
    }

    function closeBell() {
        var panel = document.getElementById('bell-panel');
        var btn = document.getElementById('bell');
        if (!panel || panel.hidden) return;
        panel.hidden = true;
        if (btn) btn.setAttribute('aria-expanded', 'false');
    }
    // site 省略 = 全部放行（多站时后台横幅上的「全部恢复」）；stage 省略 = 那一站的整站那一层。
    // `stage` 这一档框架自带的界面**不用**（卡片只做到站点级），它是留给业务自己接的：
    // 注进来的页/脚本调 `pauseCrawling('example', 'catalog')` 就能只掐那一个阶段。
    async function resumeCrawling(site, stage) {
        // 有 stage 没 site 是调用错误：老代码会把它当成「全部放行」（body 发成 {}），
        // 服务端把"无 scope"解释成全部站点、逐站清暂停 —— 一个"恢复某阶段"的手滑变成"恢复所有站"。
        // 服务端已拒绝这类参数（400），这里先拦住并说清楚，别让业务脚本踩成静默的越权放行。
        if (stage && !site) {
            Toast.err('恢复某个阶段需要同时给出站点（scope）');
            return;
        }
        var body = site ? { scope: site } : {};
        if (site && stage) body.stage = stage;
        var resp = await apiPost('/api/breaker/resume', body);
        if (!resp || !resp.ok) return;
        var data = null;
        try { data = await resp.json(); } catch (e) { /* 拿不到 body 不影响放行结果 */ }
        // resumed=false 说明本来就没闸住（比如另一个人先点了）—— 说清楚，别报个含糊的成功
        if (data && data.resumed === false) Toast.info('未变更：当前无需恢复，或探针尚未退出');
        else Toast.ok(stage ? ('已解除阶段人工暂停 ' + site + '/' + stage) : '已请求重试恢复；自动保护需通过全阶段验证后解除');
        fetchData();
    }

    async function forceResumeCrawling(site) {
        if (!site) return;
        if (!await Dialog.confirm({title:'强制解除保护',body:'<p>将跳过整站恢复验证。失败计数保留，再次失败可能立即暂停。</p>',okLabel:'强制解除'})) return;
        var resp = await apiPost('/api/breaker/force-resume', {scope:site});
        if (!resp || !resp.ok) return;
        var data = await resp.json();
        if (data.resumed) Toast.ok('已强制解除保护；未确认站点健康');
        else Toast.info('当前没有需要解除的保护');
        fetchData();
    }

    /* ---- 站点控制：一站一行（站点名 + 未暂停/已暂停 + 暂停/恢复整站） ---- */
    // 为什么要有：**站点那一层**是唯一不需要站点知识的粒度（"这一站先别抓"），也是后台**唯一**
    // 主动暂停的入口 —— 熔断横幅只在已经闸住时才出现、只有「恢复」。声明了 `StartPaused` 的站
    // 启动就停在这一档上，操作人在这里点开（见 docs/MONITOR.md §10）。
    //
    // 阶段那一层（只停某一站的某一个阶段）是**接口**：端点带 stage、引擎侧 PauseStage / ResumeStage
    // 都在，框架不替业务决定界面长什么样 —— 注进来的页/脚本调 `pauseCrawling('example', 'catalog')`
    // 就行。这里刻意不做成每个阶段一行：那是站点知识，框架没有。
    //
    // 数据：sites（跟着站点 Tab 过滤，概览 = 全部站点）+ breakers（与横幅同一份暂停状态，
    // 不额外发请求）。两个回调都没配时整块隐藏，不占位。
    function renderSiteControl(sites, breakers) {
        var sec = document.getElementById('dash-site-ctl-sec');
        var el = document.getElementById('dash-site-ctl');
        if (!sec || !el) return;
        var breakerOf = {};
        (breakers || []).forEach(function (b) { if (b) breakerOf[b.site] = b; });

        // sitePaused：这一站**整站**那一层停没停。
        // 注意 breakers 里这一站 paused=true 也可能是"某个阶段停着"（阶段级暂停也算这一站有暂停），
        // 所以要靠 source + stage 区分：**手动 + 带 stage = 阶段级**；熔断触发永远是整站那一层
        //（它的 stage 是"当时哪个阶段在失败"，不是停的范围）。
        function sitePaused(site) {
            var b = breakerOf[site];
            if (!b || !b.paused) return false;
            return !(b.source === 'manual' && b.stage);
        }

        // 站点清单由调用方给（以 sites 为准）：**每个站一行**，哪怕它没有入口阶段 ——
        // 这个「暂停/恢复」是整站的控制，不该因为"这个站没有入口"就消失。
        var list = (sites || []).slice();
        if (!list.length) { sec.hidden = true; el.innerHTML = ''; return; }

        el.innerHTML = list.map(function (site) {
            var paused = sitePaused(site);
            // 运行期值一律走 data-*（内联属性里只有固定代码）—— 与熔断那个「恢复」按钮同一套写法
            var toggle = '<button class="btn' + (paused ? '' : ' danger') + '" data-site="' + esc(site) + '" ' +
                'onclick="' + (paused ? 'resumeCrawling' : 'pauseCrawling') + '(this.dataset.site)">' +
                (paused ? '重试恢复' : '暂停整站') + '</button>';
            return '<div class="card"><div class="breaker-row">' +
                '<span class="breaker-text"><b>' + esc(siteLabel(site)) + '</b>' +
                (paused ? '｜<b>已暂停</b>' : '｜未暂停') + '</span>' +
                '<button class="btn" data-site="' + esc(site) + '" onclick="openBreakerAnalysis(this.dataset.site)">熔断分析</button>' + toggle + ((breakerOf[site] && breakerOf[site].incident_id) ? '<button class="btn danger" data-site="'+esc(site)+'" onclick="forceResumeCrawling(this.dataset.site)">强制解除保护</button>' : '') + '</div></div>';
        }).join('');
        sec.hidden = false;
    }

    var breakerAnalysisSeq = 0;
    async function openBreakerAnalysis(site) {
        var seq = ++breakerAnalysisSeq, viewCurrent = captureMonitorView(), selected = null, refreshTimer = null;
        var dialog = Dialog.open({title: site + ' · 熔断分析',
            body: '<p class="hint" id="breaker-analysis-status" role="status">正在读取熔断现场…</p>' +
                '<label>熔断记录 <select id="breaker-analysis-select" aria-label="熔断记录"></select></label>' +
                '<div id="breaker-analysis-body"></div>',
            actions: [
                {label:'复制报告',onClick:function(){if(selected && current())copyText(selected.text);return false;}},
                {label:'下载文本',onClick:function(){download('text');return false;}},
                {label:'下载 JSON',onClick:function(){download('json');return false;}},
                {label:'关闭',value:true}
            ]});
        var status = document.getElementById('breaker-analysis-status'), select = document.getElementById('breaker-analysis-select'), body = document.getElementById('breaker-analysis-body');
        var current = function(){return seq === breakerAnalysisSeq && viewCurrent() && body.isConnected;};
        dialog.then(function(){clearTimeout(refreshTimer);if(seq===breakerAnalysisSeq)breakerAnalysisSeq++;});
        async function download(format) {
            if (!selected || !current()) return;
            await downloadFile('/api/breaker/analysis?scope='+encodeURIComponent(site)+'&id='+encodeURIComponent(selected.id)+'&format='+format,current);
        }
        function render(report) {
            if (!current()) return;
            selected = report;
            var rows = report.groups.map(function(g){return '<tr><td>'+esc(g.stage)+'</td><td>'+esc(g.category)+'</td><td>'+g.count+'</td><td>'+g.percent.toFixed(1)+'%</td><td>'+g.tasks+'</td></tr>';}).join('');
            var lifecycle = report.lifecycle;
            var stateNames = {interrupted:'验证已中断',pending:'待验证',probing:'试探中',passed:'验证通过',failed:'验证失败',missing:'缺少探针',observing:'受限观察',ramping:'整站限量验证',cooldown:'整站暂停冷却',manual:'暂停，等待定时复查或人工处理',closed:'人工结束事件',recovered:'整站已恢复'};
            var actionNames = {hourly_recheck:'每小时复查',hourly_recheck_skipped:'本次复查未启动',sample_retry:'重试指定验证任务',sample_retry_unavailable:'验证任务无法重试',process_stopped:'进程正常停止',process_stop_incomplete:'进程未完全排空',process_restart:'进程重新启动',process_interrupted:'发现上次运行中断',previous_stage_snapshot:'保留上次阶段验证',history_start:'开始追加历史',history_capacity:'历史写入积压',manual_retry:'人工重试恢复',stage_probe_interrupted:'阶段验证中断',stage_probe_started:'开始阶段试探',stage_probe_passed:'阶段试探通过',stage_probe_failed:'阶段试探失败',stage_probe_missing:'阶段探针缺失',stage_task_success:'真实任务验证成功',task_failed:'真实任务失败',waiting_inflight:'等待在途任务结束',retry_after_wait:'等待服务端允许重试',manual_control:'人工操作',manual_pause:'人工暂停',manual_resume:'人工解除暂停'};
            var stageRows = lifecycle && (lifecycle.checks || []).map(function(c){return '<tr><td>'+esc(c.stage)+'</td><td>'+esc(c.state === 'passed' ? '探针/任务检查已完成' : (stateNames[c.state] || c.state))+'</td><td>'+esc(c.verification === 'readonly' ? '完整只读验证' : '真实任务验证')+'<br>'+esc(c.coverage || '未提供覆盖范围')+'</td><td>'+esc(c.verification_note || '未验证：缺少验证结论')+'<br>'+esc((c.sample_ids || []).join('，'))+'</td><td>'+Number(c.round)+'</td><td>'+Number(c.probe_successes || 0)+' / '+Number(c.task_successes || 0)+'</td><td>'+esc(c.detail || '')+'</td></tr>';}).join('');
            var timelineRows = lifecycle && (lifecycle.timeline || []).map(function(t){return '<li><b>'+esc(t.at)+'</b> · '+esc(t.stage || '整站')+' · 第 '+Number(t.round)+' 轮 · '+esc(actionNames[t.action] || stateNames[t.action] || t.action)+'<br>'+esc(t.reason || '')+(t.check ? '<br>当时验证：'+esc(t.check.verification)+'；覆盖：'+esc(t.check.coverage)+'；样本：'+esc((t.check.sample_ids || []).join('，'))+'；'+esc(t.check.verification_note) : '')+'<br>窗口失败 / 成功：'+Number(t.failures)+' / '+Number(t.successes)+(t.next_attempt_at && !t.next_attempt_at.startsWith('0001-')?'；下次计划：'+esc(t.next_attempt_at):'')+'</li>';}).join('');
            body.innerHTML = (lifecycle ? '<p><b>事件状态：</b>'+esc(stateNames[lifecycle.state] || lifecycle.state)+' · '+esc(lifecycle.stage || '整站')+'</p>'+
                (lifecycle.recheck_at && !lifecycle.recheck_at.startsWith('0001-') ? '<p>下次小时复查：'+esc(lifecycle.recheck_at)+'；恢复周期：'+Number(lifecycle.cycle || 1)+'</p>' : '')+
                '<p><b>当前结论：</b>'+esc(lifecycle.conclusion || '仍在收集证据')+'</p>'+
                '<p>成功样本：'+Number(lifecycle.successes)+'；整站试探轮数：'+Number(lifecycle.probe_attempts)+'</p>' : '')+
                (lifecycle ? '<p>触发失败：'+Number(report.failures)+'；当前窗口失败 / 成功：'+Number(lifecycle.window_failures)+' / '+Number(lifecycle.window_successes)+
                    '；短窗口失败：'+Number(lifecycle.short_failures)+'；已知失败任务：'+Number(lifecycle.distinct_tasks)+'</p>' : '')+
                '<p><strong>'+esc(report.summary)+'</strong></p>'+
                '<p>'+esc(report.complete?'样本数量与触发计数一致，未截断':'证据不完整，请结合下方说明排查')+'</p>'+
                (report.hypotheses && report.hypotheses.length ? '<p><b>结构化证据假设（未验证）：</b>'+esc(report.hypotheses.join('；'))+'</p>' : '')+
                (report.contradictions && report.contradictions.length ? '<p><b>反证：</b>'+esc(report.contradictions.join('；'))+'</p>' : '')+
                (report.missing && report.missing.length ? '<p><b>仍需验证：</b>'+esc(report.missing.join('；'))+'</p>' : '')+
                (report.groups.length ? '<p><b>优先核查（未验证）：</b>'+esc(report.groups[0].hypothesis || '')+'</p><p><b>验证步骤：</b>'+esc(report.groups[0].verify || '')+'</p>' : '')+
                '<div class="breaker-analysis-table"><table><thead><tr><th>阶段</th><th>错误类型</th><th>次数</th><th>采样占比</th><th>任务数</th></tr></thead><tbody>'+rows+'</tbody></table></div>'+
                (stageRows ? '<h3>全阶段验证进度</h3><div class="breaker-analysis-table"><table><thead><tr><th>阶段</th><th>检查进度</th><th>验证方式与覆盖范围</th><th>验证结论</th><th>轮次</th><th>探针 / 真实任务成功</th><th>说明</th></tr></thead><tbody>'+stageRows+'</tbody></table></div>' : '')+
                (lifecycle && lifecycle.checks_limited ? '<p>阶段清单不完整，不能认定整站恢复。</p>' : '')+
                (lifecycle && lifecycle.counters_reset_at && !lifecycle.counters_reset_at.startsWith('0001-') ? '<p>运行计数已于 '+esc(lifecycle.counters_reset_at)+' 清零；本报告保留清零前的统计证据。</p>' : '')+
                (timelineRows ? '<h3>运行时间线：暂停、试探与恢复</h3><ol>'+timelineRows+'</ol>' : '')+
                '<details><summary>展开完整证据与各组验证步骤</summary><pre class="log-tail-text"></pre></details>';
            body.querySelector('pre').textContent = report.text;
        }
        async function refresh() {
          if (!current()) return;
          try {
            var response = await apiFetch('/api/breaker/analysis?scope='+encodeURIComponent(site),{current:current});
            if (!current()) return;
            if (!response) throw new Error('读取失败，请检查登录状态');
            if (!response.ok) throw new Error((await response.text()).trim() || '读取熔断分析失败');
            var data = await response.json();
            if (!current()) return;
            status.textContent = data.retention;
            if (!data.reports.length) {status.textContent += ' 当前没有记录；旧版本产生的熔断无法补录实时现场。';select.disabled=true;return;}
            select.innerHTML = data.reports.map(function(r,i){return '<option value="'+i+'">'+esc(r.at)+' · '+r.failures+' 次'+(r.lifecycle?' · '+esc(r.lifecycle.state):'')+'</option>';}).join('');
            select.disabled = false;
            var selectedIndex = selected ? data.reports.findIndex(function(r){return r.id===selected.id;}) : 0;
            if (selectedIndex < 0) selectedIndex = 0;
            select.value = String(selectedIndex);
            select.onchange = function(){render(data.reports[Number(select.value)]);};
            render(data.reports[selectedIndex]);
          } catch(error) {if(current()){status.textContent=error.message;select.disabled=true;}}
          finally {if(current())refreshTimer=setTimeout(refresh,3000);}
        }
        await refresh();
    }

    // 暂停抓取：只给 site = 整站那一层（框架自带界面走的就是这一档 —— 站点控制卡上那个按钮）；
    // 再给 stage = 只停那一个阶段（留给业务自己接的界面/脚本）。暂停**必须点名**（一个误点不该停掉所有站）。
    async function pauseCrawling(site, stage) {
        var body = { scope: site };
        if (stage) body.stage = stage;
        var resp = await apiPost('/api/breaker/pause', body);
        if (!resp || !resp.ok) return;
        var data = null;
        try { data = await resp.json(); } catch (e) { /* 拿不到 body 不影响暂停结果 */ }
        if (data && data.paused === false) Toast.info(stage ? '该阶段（或整站）本来就在暂停态' : '该站点本来就在暂停态');
        else Toast.ok(stage ? ('已暂停 ' + site + '/' + stage) : ('已暂停 ' + site));
        fetchData();
    }

    /* ============ 取数 ============ */
    // API 与监控轮询由 requests.js 提供。

    /* ============ 设置 ============ */
    var settingsRequestSeq = 0, tokenRequestSeq = 0, obsListSeq = 0;
    async function fetchSettings() {
        var seq = ++settingsRequestSeq, viewCurrent = captureMonitorView();
        var current = function () { return seq === settingsRequestSeq && viewCurrent(); };
        var resp = await apiFetch('/api/settings', { current: current });
        if (!resp || !current()) return;
        var data;
        try { data = await resp.json(); } catch (_) { if (current()) Toast.err('设置响应格式无效，请刷新重试'); return; }
        if (!current()) return;
        document.getElementById('wl-input').value = (data.whitelist || []).join('\n');
        document.getElementById('wl-file-info').textContent = data.whitelist_source
            ? ('持久化在 ' + data.whitelist_source)
            : '这个宿主没接配置存储：改动仅本次运行有效';
    }
    async function saveWhitelist() {
        var current = captureMonitorView();
        var lines = document.getElementById('wl-input').value.split('\n').map(function (s) { return s.trim(); }).filter(Boolean);
        var resp = await apiPost('/api/settings/whitelist', { whitelist: lines });
        if (!current()) return;
        document.getElementById('wl-msg').textContent = resp && resp.ok ? '已保存' : '';
        if (resp && resp.ok) Toast.ok('白名单已保存');
    }
    /* ============ 访问令牌 ============ */
    /**
     * 这一页是后台自带的模块，不是 oao 表格页：表格组件的动作只回 {"status":"ok"}、不回数据，
     * 而「新增」必须把服务端生成的明文令牌交给操作人看一次（库里只存 sha256，过后无从显示）。
     */
    async function fetchTokens() {
        var el = document.getElementById('token-table');
        if (!el) return;
        var seq = ++tokenRequestSeq, viewCurrent = captureMonitorView();
        var current = function () { return seq === tokenRequestSeq && viewCurrent(); };
        el.innerHTML = '<table><tbody>' + skeletonRows(7, 3) + '</tbody></table>';
        var resp = await apiFetch('/api/tokens', { current: current });
        if (!current()) return;
        if (!resp) { el.innerHTML = '<div class="empty">读取失败，请稍后重试</div>'; return; }
        var data;
        try { data = await resp.json(); } catch (_) { if (current()) el.innerHTML = '<div class="empty">令牌响应格式无效，请刷新重试</div>'; return; }
        if (!current()) return;
        renderTokens(data.tokens || []);
    }

    function renderTokens(rows) {
        var el = document.getElementById('token-table');
        if (!rows.length) {
            el.innerHTML = '<div class="empty">还没有任何访问令牌，管理接口默认拒绝访问。'
                + '请在服务器本机执行 papa token add --operator &lt;名字&gt; 创建凭据。</div>';
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
            var update = document.createElement('button'); update.className = 'btn'; update.textContent = '更新';
            b.parentNode.insertBefore(update, b);
            update.onclick = function () { rotateToken(Number(b.dataset.tokenDel), update); };
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
        var current = captureMonitorView();
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

        if (await p !== 'ok' || !current()) return;
        var data = await tokenWrite('/api/tokens', { operator: operator.value, note: note.value }, current);
        if (!data || !data.token || !current()) return;

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
        if (current()) fetchTokens();
    }

    async function setTokenEnabled(id, enabled) {
        var current = captureMonitorView();
        if (!enabled) {
            var ok = await Dialog.confirm({
                title: '停用访问令牌',
                body: '<p>停用后这把令牌立刻失效（可以再启用）。</p>'
                    + '<p>如果它是最后一把启用中的令牌，后台将对所有人拒绝，'
                    + '那种情况只能用 <code>papa token add</code> 救回来。</p>',
                danger: true, okLabel: '停用'
            });
            if (!ok || !current()) return;
        }
        if (!await tokenWrite('/api/tokens/enabled', { id: id, enabled: enabled }, current) || !current()) return;
        Toast.ok(enabled ? '已启用' : '已停用');
        fetchTokens();
    }

    async function removeToken(id) {
        var current = captureMonitorView();
        var ok = await Dialog.confirm({
            title: '删除访问令牌',
            body: '<p>删除后无法恢复（这条令牌与它的哈希一起删掉）。</p><p>只是暂时不用的话，改用「停用」。</p>',
            danger: true, okLabel: '删除'
        });
        if (!ok || !current()) return;
        if (!await tokenWrite('/api/tokens/remove', { id: id }, current) || !current()) return;
        Toast.ok('已删除');
        fetchTokens();
    }

    /** 令牌写入复用正文超时、认证代次和服务端错误提示。 */
    async function tokenWrite(url, body, current) {
        current = current || captureMonitorView();
        if (!current()) return null;
        var resp = await apiPost(url, body);
        if (!resp || !current()) return null;
        try { var data = await resp.json(); return current() ? data : null; }
        catch (e) { if (current()) Toast.err('响应读取失败，操作结果尚未确认，请刷新后查看'); return null; }
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
        var current = captureMonitorView();
        var token = await askShutdownToken();
        if (!token || !current()) return;

        document.getElementById('shutdown-msg').textContent = '正在关闭...';
        var resp = await apiPost('/api/settings/shutdown', { token: token });
        if (!current()) return;
        document.getElementById('shutdown-msg').textContent = resp ? '已触发优雅退出' : '请求未确认，请查看提示';
        if (resp) Toast.info('已触发优雅退出');
    }

    /* ============ 日志 ============ */
    var logListSeq = 0, logViewSeq = 0, logDialogSeq = 0, logDialogOpen = false;
    function invalidateLogs() {
        logListSeq++; logViewSeq++; logDialogSeq++;
        if (logDialogOpen && typeof Dialog !== 'undefined') Dialog.close();
        logDialogOpen = false;
        ['log-list', 'log-tail-text', 'log-tail-status'].forEach(function (id) {
            var el = document.getElementById(id); if (el) el.textContent = '';
        });
    }
    async function fetchLogs() {
        var el = document.getElementById('log-list');
        if (!el || currentModule !== 'logs') return;
        var seq = ++logListSeq, epoch = authGeneration, key = loadKey();
        var current = function () { return seq === logListSeq && epoch === authGeneration && key === loadKey() && currentModule === 'logs'; };
        el.innerHTML = '<div class="hint">正在读取日志列表…</div>';
        var resp = await apiFetch('/api/logs', { current: current });
        if (!current()) return;
        if (!resp) { el.innerHTML = '<div class="hint">读取失败，请刷新重试</div>'; return; }
        var data;
        try { data = await resp.json(); }
        catch (_) { if (current()) el.innerHTML = '<div class="hint">日志列表格式无效，请刷新重试</div>'; return; }
        if (!current()) return;
        var files = Array.isArray(data.files) ? data.files : [];
        el.innerHTML = '';
        if (files.length === 0) { el.innerHTML = '<div class="hint">日志目录为空</div>'; return; }
        files.forEach(function (f) {
            var row = document.createElement('div');
            row.className = 'log-row';
            var nm = document.createElement('span');
            nm.className = 'nm'; nm.textContent = f.name;
            var sz = document.createElement('span');
            sz.className = 'sz'; sz.textContent = fmtBytes(f.size);
            var a = document.createElement('button');
            a.type = 'button'; a.className = 'log-view';
            a.textContent = '查看';
            a.onclick = function () { viewLog(f.name); };
            row.appendChild(nm); row.appendChild(sz); row.appendChild(a);
            el.appendChild(row);
        });
    }
    async function downloadFile(url, isCurrent) {
        var headers = {};
        var key = loadKey(), epoch = authGeneration;
        var current = function () { return epoch === authGeneration && key === loadKey() && (!isCurrent || isCurrent()); };
        if (key) headers['Authorization'] = 'Bearer ' + key;
        try {
        var resp = await fetch(url, { headers: headers });
        if (!current()) return;
        if (resp.status === 401) { clearKey(); showLogin(true); return; }
        if (!resp.ok) { Toast.err('下载失败 HTTP ' + resp.status); return; }
        var blob = await resp.blob();
        if (!current()) return;
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
        } catch (e) { if (current()) Toast.err('下载失败，请检查网络后重试'); }
    }
    function viewLog(name) {
        if (currentModule !== 'logs') return;
        var busy = false, seq = ++logDialogSeq, view = logViewSeq, epoch = authGeneration, key = loadKey();
        var dialog = Dialog.open({title: name + ' · 最后 200 行',
            body: '<p class="hint" id="log-tail-status" role="status">正在读取…</p><pre class="log-tail-text" id="log-tail-text"></pre>',
            actions: [{label:'刷新', onClick:function(){ refresh(); return false; }}, {label:'关闭',value:true}]});
        logDialogOpen = true;
        var status = document.getElementById('log-tail-status'), content = document.getElementById('log-tail-text');
        var current = function () { return logDialogOpen && seq === logDialogSeq && view === logViewSeq && epoch === authGeneration && key === loadKey() && currentModule === 'logs' && content.isConnected; };
        dialog.then(function () { if (seq === logDialogSeq) { logDialogOpen = false; logDialogSeq++; } });
        async function refresh() {
            if (busy || !current()) return;
            busy = true; status.textContent = '正在读取…';
            try {
                var response = await apiFetch('/api/logs/tail?file=' + encodeURIComponent(name), { current: current });
                if (!current()) return;
                if (!response) throw new Error('读取失败，请检查登录状态');
                if (!response.ok) throw new Error((await response.text()).trim() || '读取日志失败');
                var data = await response.json();
                if (!current()) return;
                content.textContent = data.text || '暂无日志';
                status.textContent = '显示最后 ' + data.lines + ' 行' + (data.truncated ? '（达到 1 MiB 上限，开头内容已截断）' : '');
                content.scrollTop = content.scrollHeight;
            } catch (error) { if (current()) { content.textContent = ''; status.textContent = error.message; } }
            finally { busy = false; }
        }
        refresh();
    }
    async function downloadLogs() {
        var view = logViewSeq;
        await downloadFile('/api/logs/download', function () { return view === logViewSeq && currentModule === 'logs'; });
    }

    /* ============ obs 报告（obs.report.dir） ============ */
    // 后端只列报告目录、把单份产物当纯文本吐回来；这里做「选报告 → 切文件 → 看原文 / 下载」。
    // 报告里会带日志与 URL（不可信文本），一律用 textContent 放进 <pre>，不渲染 markdown。
    var OBS_FILES = ['report.md', 'report.json', 'ai-brief.md', 'ai-brief.json'];
    var obsState = { dir: '', runs: [], run: '', file: '', requestSeq: 0 };

    // obsVerdict 把 report.json 里的 verdict 翻成后台的徽标；未知/读不到都算「未判定」。
    function obsVerdict(raw) {
        if (raw === 'pass') return { label: '通过', cls: 'ok' };
        if (raw === 'warn') return { label: '带风险通过', cls: 'warn' };
        if (raw === 'fail') return { label: '不通过', cls: 'err' };
        if (raw === 'unknown') return { label: '未判定', cls: 'off' };
        return { label: '未判定', cls: 'off' };
    }

    // obsFailReasons 未通过的检查（最多 3 条）：给判定单元格补一句「为什么红」。
    function obsFailReasons(ids) {
        var top = Array.isArray(ids) ? ids.slice(0, 3) : [];
        if (top.length === 0) return '';
        return ' <span class="muted" title="未通过的检查">' + esc(top.join('、')) + '</span>';
    }

    async function fetchObsReports() {
        var el = document.getElementById('obs-runs');
        if (!el) return;
        var seq = ++obsListSeq, viewCurrent = captureMonitorView();
        var current = function () { return seq === obsListSeq && viewCurrent(); };
        el.innerHTML = '<table><tbody>' + skeletonRows(5, 3) + '</tbody></table>';
        var msg = document.getElementById('obs-msg');
        if (msg) msg.textContent = '';
        var resp = await apiFetch('/api/obs/reports', { current: current });
        if (!current()) return;
        if (!resp) { el.innerHTML = '<div class="empty">读取失败，请稍后重试</div>'; return; }
        var data;
        try { data = await resp.json(); } catch (_) { if (current()) el.innerHTML = '<div class="empty">报告列表格式无效，请刷新重试</div>'; return; }
        if (current()) renderObsReports(data);
    }

    function renderObsReports(data) {
        obsState.dir = (data && data.report_dir) || '';
        obsState.runs = (data && data.runs) || [];
        var dirEl = document.getElementById('obs-dir');
        if (dirEl) {
            dirEl.textContent = obsState.dir
                ? ('报告目录：' + obsState.dir)
                : '报告目录未配置（obs.report.dir）：这份部署还没开报告产物。';
        }
        var el = document.getElementById('obs-runs');
        if (!el) return;
        if (obsState.runs.length === 0) {
            el.innerHTML = '<div class="empty">还没有报告：跑一次 <code>papa report</code>，'
                + '或开启 obs.report.on_exit 让进程退出时自动生成。</div>';
            return;
        }
        var html = '<table><thead><tr><th>报告</th><th>判定</th><th>生成时间</th><th>文件</th><th>操作</th></tr></thead><tbody>';
        obsState.runs.forEach(function (run) {
            var v = obsVerdict(run.verdict);
            html += '<tr>'
                + '<td><b>' + esc(run.name) + '</b></td>'
                + '<td><span class="badge ' + v.cls + '">' + v.label + '</span>'
                + (run.mode === 'snapshot' ? ' <span class="badge off">快照</span>' : '')
                + (run.engine_paused ? ' <span class="badge warn">引擎暂停</span>' : '')
                + obsFailReasons(run.fail_reason_top)
                + '</td>'
                + '<td>' + esc(run.generated_at ? fmtTime(run.generated_at) : fmtTime(run.mod_time)) + '</td>'
                + '<td>' + ((run.files || []).length) + ' / ' + OBS_FILES.length + '</td>'
                + '<td><button class="btn" style="margin-top:0" data-run="' + esc(run.name)
                + '" onclick="openObsReport(this.dataset.run)">查看</button></td>'
                + '</tr>';
        });
        el.innerHTML = html + '</tbody></table>';
    }

    // openObsReport 选中一份报告：默认打开 report.md（缺它时退到第一份存在的产物）。
    function openObsReport(runName) {
        var run = obsState.runs.filter(function (r) { return r.name === runName; })[0];
        if (!run) return;
        obsState.run = run.name;
        var names = (run.files || []).map(function (f) { return f.name; });
        obsState.file = names.indexOf('report.md') >= 0 ? 'report.md' : (names[0] || '');
        document.getElementById('obs-viewer-sec').hidden = false;
        renderObsTabs();
        loadObsReport();
    }

    function renderObsTabs() {
        var tabs = document.getElementById('obs-tabs');
        if (!tabs) return;
        var run = obsState.runs.filter(function (r) { return r.name === obsState.run; })[0];
        var names = run ? (run.files || []).map(function (f) { return f.name; }) : [];
        tabs.innerHTML = names.map(function (name) {
            var active = name === obsState.file ? ' active' : '';
            return '<button class="obs-tab' + active + '" data-obs-file="' + esc(name) + '">' + esc(name) + '</button>';
        }).join('');
        tabs.querySelectorAll('[data-obs-file]').forEach(function (b) {
            b.onclick = function () { obsState.file = b.dataset.obsFile; renderObsTabs(); loadObsReport(); };
        });
        var meta = document.getElementById('obs-meta');
        if (meta) meta.textContent = obsState.run ? ('报告：' + obsState.run) : '';
    }

    async function loadObsReport() {
        // 每次选择都作废旧请求，包括没有产物的报告。
        var seq = ++obsState.requestSeq;
        var viewCurrent = captureMonitorView();
        var current = function () { return seq === obsState.requestSeq && viewCurrent(); };
        var pre = document.getElementById('obs-content');
        if (!pre) return;
        if (!obsState.run || !obsState.file) { pre.textContent = '这份报告没有可看的产物'; return; }
        pre.textContent = '加载中…';
        var url = '/api/obs/report?run=' + encodeURIComponent(obsState.run)
            + '&file=' + encodeURIComponent(obsState.file);
        try {
            var resp = await apiFetch(url, { current: current });
            if (!current()) return;
            if (!resp) { pre.textContent = '读取失败（报告可能已被清理）'; return; }
            var content = await resp.text();
            // 响应头先到不代表正文已经读完，读取正文期间也可能切换。
            if (!current()) return;
            pre.textContent = content;
        } catch (_) {
            if (current()) pre.textContent = '读取失败（报告可能已被清理）';
        }
    }

    async function downloadObsReport() {
        if (!obsState.run || !obsState.file) return;
        var current = captureMonitorView(), seq = obsState.requestSeq;
        var url = '/api/obs/report?run=' + encodeURIComponent(obsState.run)
            + '&file=' + encodeURIComponent(obsState.file) + '&download=1';
        await downloadFile(url, function () { return current() && seq === obsState.requestSeq; });
    }

    /* ============ 启动 ============ */
    document.getElementById('key').addEventListener('keydown', function (e) { if (e.key === 'Enter') submitKey(); });

    // 铃铛：点面板外部、按 Esc 关掉它（注册一次就够，开合状态在 toggleBell / closeBell 里）。
    // 点铃铛自己与面板里的按钮都算"内部"—— 否则点「恢复」时面板会先被关掉（虽然请求照发，
    // 但列表会在下一轮刷新前空一下）。
    document.addEventListener('click', function (ev) {
        var wrap = document.getElementById('bell-wrap');
        if (wrap && ev.target && wrap.contains(ev.target)) return;
        closeBell();
    });
    document.addEventListener('keydown', function (ev) {
        if (ev.key !== 'Escape') return;
        closeBell();
        var toggle = document.getElementById('nav-toggle');
        if (toggle && toggle.getAttribute('aria-expanded') === 'true') { setNavigationOpen(false); toggle.focus(); }
    });

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
        onUnauthorized: handleTableUnauthorized
    });
    installLegacyTableAdapter();

    if (typeof syncMonitorNavigation === 'function') syncMonitorNavigation();

    // 有本地凭据或宿主使用自定义登录界面时开始加载；默认无凭据时停在登录框，
    // 等登录成功后再开始轮询/渲染菜单（避免未登录状态下每 3 秒刷一次 401）。
    if (authRequired() && !loadKey()) {
        showLogin(false);
    } else {
        hideLogin();
        startPolling();
    }

async function rotateToken(id, button) {
    if (button.disabled) return;
    button.disabled = true;
    var current = captureMonitorView();
    try {
        if (!await Dialog.confirm({title:'更新访问令牌',body:'<p>更新后旧令牌立即失效。新令牌只显示一次，请及时保存。响应丢失时可用本机 papa token add 恢复访问。</p>',okLabel:'更新'}) || !current()) return;
        stopPolling();
        var data = await tokenWrite('/api/tokens/rotate', {id:id}, current);
        if (!data || !data.token || !current()) return;
        if (data.current) saveKey(data.token);
        await Dialog.open({title:'新令牌（只显示一次）',body:'<div class="secret-box">'+esc(data.token)+'</div>',actions:[
            {label:'复制',onClick:function(){copyText(data.token);return false;}},{label:'已保存',value:true}
        ]});
        fetchTokens();
    } finally { button.disabled = false; startPolling(); }
}
