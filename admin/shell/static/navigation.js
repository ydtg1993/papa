/* shell/navigation: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/navigation",null,["document","getActiveModule","getCurrentPage","getCurrentTable","getSelectedSite","openPage","openTable","setNavigationOpen","switchModule"],function(deps,lifecycle){
'use strict';
/* Papa owns four navigation sections; oao remains a generic table component. */
'use strict';
var MONITOR_SECTIONS = { flow: '采集任务流', monitor: '运行监控与报告', data: '数据与资源', system: '系统管理' };
var currentSection = 'monitor';
var monitorNavigation = { tables: [], pages: [] };
var MONITOR_MODULE_NAV = [
    { key: 'dashboard', label: '运行概览', section: 'monitor' },
    { key: 'obs', label: '运行报告', section: 'monitor' },
    { key: 'logs', label: '运行日志', section: 'monitor' },
    { key: 'queues', label: '队列治理', section: 'monitor' },
    { key: 'settings', label: '系统设置', section: 'system' },
    { key: 'tokens', label: '访问令牌', section: 'system' }
];

function monitorSection(section) {
    return typeof section === 'string' && Object.prototype.hasOwnProperty.call(MONITOR_SECTIONS, section) ? section : 'data';
}
function monitorTableSection(key) { return key === 'operation_log' ? 'system' : 'data'; }
function monitorTableSupportsSite(key) {
    var table = monitorNavigation.tables.filter(function (item) { return item.key === key; })[0];
    // The task table has a stable framework-owned site filter even before menus arrive.
    return table ? Array.isArray(table.filters) && table.filters.some(function (filter) { return filter && filter.name === 'site'; }) : key === 'task';
}
function monitorNavigationOwner(kind,key){var owner=null;for(var i=0;i<monitorNavigation.pages.length;i++){var next=monitorNavigation.pages.find(function(p){return (p.owns||[]).indexOf(kind+':'+key)>=0;});if(!next)return owner;owner=next;kind='page';key=next.key;}return owner;}
function monitorNavigationItems() {
    var tables = monitorNavigation.tables.map(function (item) {
        return { kind: 'table', key: item.key, label: item.key === 'task' ? '采集任务（crawler_tasks）' : item.label,
            group: item.key === 'task' || item.key === 'operation_log' ? '' : item.group, section: monitorTableSection(item.key) };
    });
    var pages = monitorNavigation.pages.map(function (item) {
        return { kind: 'page', key: item.key, label: item.label, group: item.group, section: monitorSection(item.section) };
    });
    // Business workbenches precede flow operations; task operations belong to data.
    return pages.filter(function (item) { return item.section === 'flow'; })
        .concat(tables.filter(function (item) { return item.key === 'task'; }))
        .concat(MONITOR_MODULE_NAV.map(function (item) { return Object.assign({ kind: 'module' }, item); }))
        .concat(tables.filter(function (item) { return item.key !== 'task'; }))
        .concat(pages.filter(function (item) { return item.section !== 'flow'; }))
        .filter(function(item){return !monitorNavigationOwner(item.kind,item.key);});
}
function setMonitorNavigation(tables, pages) {
    function clean(items) {
        var seen = Object.create(null);
        return (Array.isArray(items) ? items : []).filter(function (item) {
            if (!item || typeof item.key !== 'string' || !item.key || seen[item.key]) return false;
            seen[item.key] = true; return true;
        }).map(function (item) {
            return { key: item.key, label: typeof item.label === 'string' && item.label ? item.label : item.key,
                group: typeof item.group === 'string' ? item.group : '', section: monitorSection(item.section), filters: item.filters, owns: Array.isArray(item.owns)?item.owns.filter(function(x){return typeof x==='string'&&/^(module|page):[a-zA-Z0-9_-]+$/.test(x);}):[],
                frameworkData: typeof item.header === 'string' && item.header.indexOf('data-papa-framework-table') !== -1 };
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
    var kind = deps.getActiveModule() === 'tables' ? 'table' : deps.getActiveModule() === 'pages' ? 'page' : 'module';
    var key = kind === 'table' ? deps.getCurrentTable() : kind === 'page' ? deps.getCurrentPage() && deps.getCurrentPage().key : deps.getActiveModule();
    var item = monitorNavigationItems().filter(function (entry) { return entry.kind === kind && entry.key === key; })[0];
    if (item) return item;
    var owner=monitorNavigationOwner(kind,key);if(owner)return {kind:'page',key:owner.key,label:owner.label,section:owner.section};
    return { kind: kind, key: key, section: deps.getActiveModule() === 'section' ? currentSection : kind === 'table' ? monitorTableSection(key) : 'data' };
}
function monitorSiteScoped() {
    return ['dashboard', 'queues'].indexOf(deps.getActiveModule()) >= 0 ||
        (deps.getActiveModule() === 'tables' && monitorTableSupportsSite(deps.getCurrentTable()));
}
function syncMonitorSiteScope() {
    var scoped = monitorSiteScoped();
    var tabs = deps.document.getElementById('site-tabs'), meta = deps.document.getElementById('site-meta');
    if (tabs) tabs.hidden = !scoped || !tabs.childElementCount;
    if (meta) meta.hidden = !scoped || !meta.innerHTML || deps.getSelectedSite() === null;
}
function syncMonitorNavigation(focus) {
    var active = monitorActiveNavigation();
    currentSection = monitorSection(active.section);
    deps.document.querySelectorAll('.nav-category').forEach(function (button) {
        var selected = button.dataset.section === currentSection;
        button.classList.toggle('active', selected);
        button.setAttribute('aria-current', selected ? 'true' : 'false');
    });
    var title = deps.document.getElementById('section-title');
    if (title) title.textContent = MONITOR_SECTIONS[currentSection];
    var moduleTitle = deps.document.getElementById('moduleTitle');
    if (moduleTitle && active.label) moduleTitle.textContent = active.label;
    var nav = deps.document.getElementById('section-nav');
    if (nav) {
        var oldFocus = nav.contains(deps.document.activeElement) && deps.document.activeElement.dataset ? deps.document.activeElement.dataset.navigationKey : null;
        nav.replaceChildren();
        var groups = [], entries = monitorNavigationItems().filter(function (item) { return item.section === currentSection; });
        entries.forEach(function (item) { var group = item.group || ''; if (groups.indexOf(group) < 0) groups.push(group); });
        groups.forEach(function (group) {
            if (group) { var heading = deps.document.createElement('span'); heading.className = 'nav-section'; heading.textContent = group; nav.appendChild(heading); }
            entries.filter(function (item) { return (item.group || '') === group; }).forEach(function (item) {
                var button = deps.document.createElement('button'), selected = item.kind === active.kind && item.key === active.key;
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
        var empty = deps.document.getElementById('section-empty');
        if (empty && deps.getActiveModule() === 'section') {
            empty.textContent = entries.length ? '请选择上方入口。' : '此分类暂无已注册入口。业务数据表和未指定分类的自定义页会显示在数据与资源中。';
        }
    }
    syncMonitorSiteScope();
    if (focus && moduleTitle && moduleTitle.focus) moduleTitle.focus();
}
function openMonitorNavigationItem(item) {
    if (item.kind === 'table') return deps.openTable(item.key, item.label);
    if (item.kind === 'page') return deps.openPage(item.key, item.label);
    return deps.switchModule(item.key);
}
function switchSection(section) {
    section = monitorSection(section);
    var items = monitorNavigationItems().filter(function (item) { return item.section === section; });
    if (currentSection === section && deps.getActiveModule() !== 'section') { deps.setNavigationOpen(false); syncMonitorNavigation(true); return true; }
    if (items.length) return openMonitorNavigationItem(items[0]);
    return deps.switchModule('section', { section: section, label: MONITOR_SECTIONS[section] });
}

function reset(){resetMonitorNavigation();}

function getNavigationSection(){return currentSection;} function selectNavigationSection(section){currentSection=section;} function getNavigationTables(){return monitorNavigation.tables.slice();}

return Object.defineProperties({}, {
"monitorSection":{enumerable:true,get:function(){return monitorSection;},set:function(value){monitorSection=value;}},
"monitorTableSection":{enumerable:true,get:function(){return monitorTableSection;},set:function(value){monitorTableSection=value;}},
"monitorTableSupportsSite":{enumerable:true,get:function(){return monitorTableSupportsSite;},set:function(value){monitorTableSupportsSite=value;}},
"monitorNavigationOwner":{enumerable:true,get:function(){return monitorNavigationOwner;},set:function(value){monitorNavigationOwner=value;}},
"monitorNavigationItems":{enumerable:true,get:function(){return monitorNavigationItems;},set:function(value){monitorNavigationItems=value;}},
"setMonitorNavigation":{enumerable:true,get:function(){return setMonitorNavigation;},set:function(value){setMonitorNavigation=value;}},
"resetMonitorNavigation":{enumerable:true,get:function(){return resetMonitorNavigation;},set:function(value){resetMonitorNavigation=value;}},
"monitorActiveNavigation":{enumerable:true,get:function(){return monitorActiveNavigation;},set:function(value){monitorActiveNavigation=value;}},
"monitorSiteScoped":{enumerable:true,get:function(){return monitorSiteScoped;},set:function(value){monitorSiteScoped=value;}},
"syncMonitorSiteScope":{enumerable:true,get:function(){return syncMonitorSiteScope;},set:function(value){syncMonitorSiteScope=value;}},
"syncMonitorNavigation":{enumerable:true,get:function(){return syncMonitorNavigation;},set:function(value){syncMonitorNavigation=value;}},
"openMonitorNavigationItem":{enumerable:true,get:function(){return openMonitorNavigationItem;},set:function(value){openMonitorNavigationItem=value;}},
"switchSection":{enumerable:true,get:function(){return switchSection;},set:function(value){switchSection=value;}},
"reset":{enumerable:true,get:function(){return reset;},set:function(value){reset=value;}},
"getNavigationSection":{enumerable:true,get:function(){return getNavigationSection;},set:function(value){getNavigationSection=value;}},
"selectNavigationSection":{enumerable:true,get:function(){return selectNavigationSection;},set:function(value){selectNavigationSection=value;}},
"getNavigationTables":{enumerable:true,get:function(){return getNavigationTables;},set:function(value){getNavigationTables=value;}}
});
});
MonitorShell.install("shell/navigation");
