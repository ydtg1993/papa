/* Papa owns four navigation sections; oao remains a generic table component. */
'use strict';
var MONITOR_SECTIONS = { flow: '采集任务流', monitor: '运行监控与报告', data: '数据与资源', system: '系统管理' };
var currentSection = 'monitor';
var monitorNavigation = { tables: [], pages: [] };
var MONITOR_MODULE_NAV = [
    { key: 'dashboard', label: '运行概览', section: 'monitor' },
    { key: 'quality', label: '运行质量', section: 'monitor' },
    { key: 'obs', label: '运行报告', section: 'monitor' },
    { key: 'logs', label: '运行日志', section: 'monitor' },
    { key: 'schedules', label: '周期批次', section: 'flow' },
    { key: 'rules', label: '规则与发布', section: 'flow' },
    { key: 'agents', label: 'Agent 候选', section: 'flow' },
    { key: 'queues', label: '异常处理', section: 'flow' },
    { key: 'settings', label: '系统设置', section: 'system' },
    { key: 'tokens', label: '访问令牌', section: 'system' }
];

function monitorSection(section) {
    return typeof section === 'string' && Object.prototype.hasOwnProperty.call(MONITOR_SECTIONS, section) ? section : 'data';
}
function monitorTableSection(key) { return key === 'task' ? 'flow' : key === 'operation_log' ? 'system' : 'data'; }
function monitorTableSupportsSite(key) {
    var table = monitorNavigation.tables.filter(function (item) { return item.key === key; })[0];
    // The task table has a stable framework-owned site filter even before menus arrive.
    return table ? Array.isArray(table.filters) && table.filters.some(function (filter) { return filter && filter.name === 'site'; }) : key === 'task';
}
function monitorNavigationItems() {
    var tables = monitorNavigation.tables.map(function (item) {
        return { kind: 'table', key: item.key, label: item.key === 'task' ? '采集任务' : item.label,
            group: item.key === 'task' || item.key === 'operation_log' ? '' : item.group, section: monitorTableSection(item.key) };
    });
    var pages = monitorNavigation.pages.map(function (item) {
        return { kind: 'page', key: item.key, label: item.label, group: item.group, section: monitorSection(item.section) };
    });
    // A business workbench and the framework task table precede flow operations.
    return pages.filter(function (item) { return item.section === 'flow'; })
        .concat(tables.filter(function (item) { return item.key === 'task'; }))
        .concat(MONITOR_MODULE_NAV.map(function (item) { return Object.assign({ kind: 'module' }, item); }))
        .concat(tables.filter(function (item) { return item.key !== 'task'; }))
        .concat(pages.filter(function (item) { return item.section !== 'flow'; }));
}
function setMonitorNavigation(tables, pages) {
    function clean(items) {
        var seen = Object.create(null);
        return (Array.isArray(items) ? items : []).filter(function (item) {
            if (!item || typeof item.key !== 'string' || !item.key || seen[item.key]) return false;
            seen[item.key] = true; return true;
        }).map(function (item) {
            return { key: item.key, label: typeof item.label === 'string' && item.label ? item.label : item.key,
                group: typeof item.group === 'string' ? item.group : '', section: monitorSection(item.section), filters: item.filters };
        });
    }
    monitorNavigation.tables = clean(tables); monitorNavigation.pages = clean(pages);
    syncMonitorNavigation();
}
function resetMonitorNavigation() {
    monitorNavigation.tables = []; monitorNavigation.pages = [];
    syncMonitorNavigation();
}
function monitorActiveNavigation() {
    var kind = currentModule === 'tables' ? 'table' : currentModule === 'pages' ? 'page' : 'module';
    var key = kind === 'table' ? currentTable : kind === 'page' ? currentPage && currentPage.key : currentModule;
    var item = monitorNavigationItems().filter(function (entry) { return entry.kind === kind && entry.key === key; })[0];
    if (item) return item;
    return { kind: kind, key: key, section: currentModule === 'section' ? currentSection : kind === 'table' ? monitorTableSection(key) : 'data' };
}
function monitorSiteScoped() {
    return ['dashboard', 'schedules', 'rules', 'quality', 'agents'].indexOf(currentModule) >= 0 ||
        (currentModule === 'tables' && monitorTableSupportsSite(currentTable));
}
function syncMonitorSiteScope() {
    var scoped = monitorSiteScoped();
    var tabs = document.getElementById('site-tabs'), meta = document.getElementById('site-meta');
    if (tabs) tabs.hidden = !scoped || !tabs.childElementCount;
    if (meta) meta.hidden = !scoped || !meta.innerHTML || currentSite === null;
}
function syncMonitorNavigation(focus) {
    var active = monitorActiveNavigation();
    currentSection = monitorSection(active.section);
    document.querySelectorAll('.nav-category').forEach(function (button) {
        var selected = button.dataset.section === currentSection;
        button.classList.toggle('active', selected);
        button.setAttribute('aria-current', selected ? 'true' : 'false');
    });
    var title = document.getElementById('section-title');
    if (title) title.textContent = MONITOR_SECTIONS[currentSection];
    var moduleTitle = document.getElementById('moduleTitle');
    if (moduleTitle && active.label) moduleTitle.textContent = active.label;
    var nav = document.getElementById('section-nav');
    if (nav) {
        var oldFocus = nav.contains(document.activeElement) && document.activeElement.dataset ? document.activeElement.dataset.navigationKey : null;
        nav.replaceChildren();
        var groups = [], entries = monitorNavigationItems().filter(function (item) { return item.section === currentSection; });
        entries.forEach(function (item) { var group = item.group || ''; if (groups.indexOf(group) < 0) groups.push(group); });
        groups.forEach(function (group) {
            if (group) { var heading = document.createElement('span'); heading.className = 'nav-section'; heading.textContent = group; nav.appendChild(heading); }
            entries.filter(function (item) { return (item.group || '') === group; }).forEach(function (item) {
                var button = document.createElement('button'), selected = item.kind === active.kind && item.key === active.key;
                button.type = 'button'; button.className = 'nav-item'; button.classList.toggle('active', selected);
                button.textContent = item.label; button.dataset.navigationKey = item.kind + ':' + item.key;
                if (item.kind === 'module') button.dataset.module = item.key;
                else if (item.kind === 'table') button.dataset.oao = item.key;
                else button.dataset.page = item.key;
                button.setAttribute('aria-current', selected ? 'page' : 'false');
                button.onclick = function () { openMonitorNavigationItem(item); };
                nav.appendChild(button);
                if (!focus && oldFocus === button.dataset.navigationKey) button.focus();
            });
        });
        var empty = document.getElementById('section-empty');
        if (empty && currentModule === 'section') {
            empty.textContent = entries.length ? '请选择上方入口。' : '此分类暂无已注册入口。业务数据表和未指定分类的自定义页会显示在数据与资源中。';
        }
    }
    syncMonitorSiteScope();
    if (focus && moduleTitle && moduleTitle.focus) moduleTitle.focus();
}
function openMonitorNavigationItem(item) {
    if (item.kind === 'table') return openTable(item.key, item.label);
    if (item.kind === 'page') return openPage(item.key, item.label);
    return switchModule(item.key);
}
function switchSection(section) {
    section = monitorSection(section);
    var items = monitorNavigationItems().filter(function (item) { return item.section === section; });
    if (currentSection === section && currentModule !== 'section') { setNavigationOpen(false); syncMonitorNavigation(true); return true; }
    if (items.length) return openMonitorNavigationItem(items[0]);
    return switchModule('section', { section: section, label: MONITOR_SECTIONS[section] });
}
