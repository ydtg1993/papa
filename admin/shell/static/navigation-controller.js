/* shell/navigation-controller: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/navigation-controller",null,["Dialog","MonitorShell","Oao","Papa","apiFetch","destroyMonitorVisuals","document","esc","fetchLogs","fetchObsReports","fetchSettings","fetchTokens","getActiveModule","getMonitorSnapshot","getNavigationSection","getNavigationTables","getSelectedSite","getSessionGeneration","getTableViewRevision","invalidateLogs","invalidateMonitorView","loadKey","moduleTitle","monitorTableSupportsSite","nextTableView","renderMonitorVisuals","renderQueues","retireTableView","selectNavigationSection","setActiveModule","setMonitorNavigation","syncMonitorNavigation"],function(deps,lifecycle){
'use strict';
    function switchModule(name, view) {
        var target = deps.document.getElementById('mod-' + name);
        if (!target) return false;
        if (typeof deps.Papa !== 'undefined' && !deps.Papa.leave(false)) return false;
        deps.invalidateMonitorView();
        if (typeof deps.Dialog !== 'undefined' && typeof deps.Dialog.reset === 'function') deps.Dialog.reset();
        if (typeof deps.destroyMonitorVisuals === 'function') deps.destroyMonitorVisuals();
        if (deps.getActiveModule() === 'tables') deps.retireTableView();
        setNavigationOpen(false);
        if (typeof deps.invalidateLogs === 'function') deps.invalidateLogs();
        deps.MonitorShell.transition(name);
        deps.setActiveModule(name);
        if (view && view.table) currentTable = view.table;
        if (view && view.page) currentPage = { key: view.page, label: view.label };
        if (view && view.section && typeof deps.getNavigationSection() !== 'undefined') deps.selectNavigationSection(view.section);
        deps.document.querySelectorAll('.module').forEach(function (m) { m.classList.remove('active'); });
        deps.document.getElementById('mod-' + name).classList.add('active');
        deps.document.querySelectorAll('.nav-item').forEach(function (b) {
            b.classList.toggle('active', b.dataset.module === name);
        });
        deps.document.getElementById('moduleTitle').textContent = view && view.label || deps.moduleTitle(name);
        if (typeof deps.Oao !== 'undefined' && deps.Oao.motion) deps.Oao.motion.animate(deps.document.getElementById('mod-' + name), 'enter');
        if (name === 'dashboard' && deps.getMonitorSnapshot() && typeof deps.renderMonitorVisuals === 'function') deps.renderMonitorVisuals(deps.getMonitorSnapshot());
        if (name === 'settings') { deps.Papa.renderSettings(deps.document.getElementById('custom-settings')); deps.fetchSettings(); }
        if (name === 'logs') { deps.fetchLogs(); }
        if (name === 'tokens') { deps.fetchTokens(); }
        if (name === 'obs') { deps.fetchObsReports(); }
        if (name === 'queues') {
            var queueMessage = deps.document.getElementById('queue-gov-msg');
            if (queueMessage) queueMessage.textContent = '';
            if (deps.getMonitorSnapshot()) deps.renderQueues(deps.getMonitorSnapshot().queues);
        }
        if (typeof deps.syncMonitorNavigation === 'function') deps.syncMonitorNavigation(true);
        return true;
    }

    function setNavigationOpen(open) {
        var nav = deps.document.getElementById('main-nav'), toggle = deps.document.getElementById('nav-toggle');
        if (nav) nav.classList.toggle('nav-open', open);
        if (toggle) toggle.setAttribute('aria-expanded', String(open));
        if (open && nav) { var active = nav.querySelector('.nav-category.active') || nav.querySelector('.nav-category'); if (active) active.focus(); }
    }
    function toggleNavigation() {
        setNavigationOpen(deps.document.getElementById('nav-toggle').getAttribute('aria-expanded') !== 'true');
    }

    /* ============ 表格（oao 组件） ============ */
    var currentTable = null; // 当前打开的表 key（切站点 tab 时要按新站点重画它）

    /**
     * tableFilterOpts 让表格跟着**当前站点 tab** 走：切到某站再打开任务表，就带着 `site=<该站>` 的
     * 预置筛选打开（oao 会把它填进筛选栏，用户能改能清）。概览不预设 —— 它本来就是看全部。
     */
    function tableFilterOpts() {
        if (deps.getSelectedSite() === null || (typeof deps.monitorTableSupportsSite === 'function' && !deps.monitorTableSupportsSite(currentTable))) return { filter: {} };
        return { filter: { site: deps.getSelectedSite() } };
    }

    /** 打开某张表：切到表格模块、点亮对应菜单项，再让 oao 渲染 */
    async function openTable(key, label, btn, initialFilter) {
        if (!switchModule('tables', { table: key, label: label || '数据' })) return false;
        await renderTableView(key, initialFilter);
        return true;
    }

    async function renderTableView(key, initialFilter) {
        // 旧 oao 没有销毁/请求代次保护，隔离旧节点使其晚到渲染无法写入新视图。
        if (typeof deps.Oao.destroy !== 'function') deps.retireTableView();
        var view = deps.document.getElementById('oao-view'), epoch = deps.nextTableView(), identity = deps.getSessionGeneration();
        var credential = deps.loadKey();
        var current = function () { return epoch === deps.getTableViewRevision() && identity === deps.getSessionGeneration() && credential === deps.loadKey() && deps.getActiveModule() === 'tables' && currentTable === key && view === deps.document.getElementById('oao-view'); };
        try {
            // A server-owned marker distinguishes catalog tables from explicitly registered business tables.
            var entry = typeof deps.getNavigationTables === 'function' && deps.getNavigationTables().find(function (item) { return item.key === key; });
            if (entry && entry.frameworkData) {
                if (typeof deps.Oao.destroy === 'function') deps.Oao.destroy(view);
                view.textContent = '正在检查框架表…';
                var response = await deps.apiFetch('/api/framework-tables/' + encodeURIComponent(key), { current: current });
                if (!current()) return;
                if (!response || !response.ok) { view.textContent = '框架表状态读取失败，请重新打开此表重试。'; return; }
                var status = await response.json();
                if (!current()) return;
                if (status.state !== 'available') { view.textContent = status.message || '框架表暂不可用，请重新打开此表重试。'; return; }
            }
            // 第三个参数 = oao 的预置筛选（需要 oao ≥ 带 opts.filter 的那版；老版本会忽略它）
            await deps.Oao.render(view, key, initialFilter ? { filter: Object.assign({}, tableFilterOpts().filter, initialFilter) } : tableFilterOpts());
        } catch (e) {
            if (current()) {
                view.innerHTML = '<div class="empty">表格加载失败：' + deps.esc(e && e.message || '请求失败') + '</div>';
            }
        }
    }

    /** 按 Group 分组渲染当前分类的动态菜单；固定分类不依赖 oao 的挂载节点。 */
    var navRequestSeq = 0;
    async function renderNav() {
        var seq = ++navRequestSeq, epoch = deps.getSessionGeneration(), key = deps.loadKey();
        var current = function () { return seq === navRequestSeq && epoch === deps.getSessionGeneration() && key === deps.loadKey(); };
        var tables = [];
        try { tables = (await deps.Oao.list(true)) || []; } catch (e) { tables = []; }
        if (!current()) return;
        var pages = [], resp = await deps.apiFetch('/api/pages', { current: current });
        if (resp) {
            try { pages = (await resp.json()).pages || []; } catch (e) { pages = []; }
        }
        if (!current()) return;
        if (typeof deps.setMonitorNavigation === 'function') deps.setMonitorNavigation(tables, pages);
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
        var view = deps.document.getElementById('page-view');
        view.innerHTML = '';
        if (!deps.Papa.render(key, view, { key: key, label: label })) {
            view.innerHTML = '<div class="empty">页面 ' + deps.esc(key)
                + ' 没有注册渲染函数：检查 app.UsePage 的 Script 里是否调了 Papa.page("'
                + deps.esc(key) + '", fn)</div>';
        }
        return true;
    }


function getCurrentTable(){return currentTable;} function getCurrentPage(){return currentPage ? Object.assign({},currentPage):null;}

return Object.defineProperties({}, {
"switchModule":{enumerable:true,get:function(){return switchModule;},set:function(value){switchModule=value;}},
"setNavigationOpen":{enumerable:true,get:function(){return setNavigationOpen;},set:function(value){setNavigationOpen=value;}},
"toggleNavigation":{enumerable:true,get:function(){return toggleNavigation;},set:function(value){toggleNavigation=value;}},
"tableFilterOpts":{enumerable:true,get:function(){return tableFilterOpts;},set:function(value){tableFilterOpts=value;}},
"openTable":{enumerable:true,get:function(){return openTable;},set:function(value){openTable=value;}},
"renderTableView":{enumerable:true,get:function(){return renderTableView;},set:function(value){renderTableView=value;}},
"renderNav":{enumerable:true,get:function(){return renderNav;},set:function(value){renderNav=value;}},
"openPage":{enumerable:true,get:function(){return openPage;},set:function(value){openPage=value;}},
"getCurrentTable":{enumerable:true,get:function(){return getCurrentTable;},set:function(value){getCurrentTable=value;}},
"getCurrentPage":{enumerable:true,get:function(){return getCurrentPage;},set:function(value){getCurrentPage=value;}}
});
});
MonitorShell.install("shell/navigation-controller");
