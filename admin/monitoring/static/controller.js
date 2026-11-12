/* monitoring/controller: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("monitoring/controller","dashboard",["MonitorObservations","bar","buildWorkbench","document","esc","fmtAgo","fmtBytes","fmtDuration","getActiveModule","getCurrentTable","monitorTableSupportsSite","pct","renderBell","renderMonitorVisuals","renderQueueGovernance","renderQueues","renderSiteControl","renderTableView","renderWorkbench","syncMonitorSiteScope","workbenchCount","workbenchNumber"],function(deps,lifecycle){
'use strict';
var siteTabsHTML=new WeakMap();
    /* ============ 渲染 ============ */
    function renderSystem(s, generatedAt) {
        var el = deps.document.getElementById('dash-sys');
        if (!s) { el.innerHTML = '<div class="empty">无系统指标</div>'; return; }
        el.innerHTML = ['cpu','mem','disk'].map(function (key) {
            var v = deps.MonitorObservations.resource(s,key,deps.MonitorObservations.time(generatedAt));
            var label = {cpu:'CPU',mem:'内存',disk:'磁盘'}[key], valid = v.value !== null;
            return '<div class="card"><div class="label">' + label + (key === 'disk' ? ' · 根文件系统' : ' · 系统采样') + '</div><div class="value">' + (valid ? v.value.toFixed(1) + '%' : '—') + '</div>'
                + '<div class="sub">' + (valid && key!=='cpu' ? deps.fmtBytes(s[key+'_used']) + ' / ' + deps.fmtBytes(s[key+'_total']) : v.state) + '</div>'
                + (valid ? deps.bar(key,deps.pct(v.value)) : '') + '</div>';
        }).join('');
    }
    function renderDirs(dirs) {
        var el = deps.document.getElementById('dash-dirs');
        var keys = Object.keys(dirs || {});
        if (keys.length === 0) { el.innerHTML = '<div class="empty">未配置 monitor_dirs</div>'; return; }
        el.innerHTML = keys.map(function (name) {
            var d = dirs[name];
            return '<div class="card"><div class="label">' + deps.esc(name) + '</div><div class="value">' + deps.fmtBytes(d.size) + '</div><div class="sub">' + deps.esc(d.path || '') + '</div></div>';
        }).join('');
    }
    function stageTiles(q) {
        q = q || {};
        return '<div class="tiles">'
            + '<div class="tile queued"><div class="k">排队</div><div class="v">' + deps.workbenchNumber(deps.workbenchCount(q.queue_len)) + '</div></div>'
            + '<div class="tile running"><div class="k">未完成（含排队）</div><div class="v">' + deps.workbenchNumber(deps.workbenchCount(q.in_progress)) + '</div></div>'
            + '<div class="tile done"><div class="k">完成</div><div class="v">' + deps.workbenchNumber(deps.workbenchCount(q.completed)) + '</div></div>'
            + '<div class="tile failed"><div class="k">错误</div><div class="v">' + deps.workbenchNumber(deps.workbenchCount(q.failed)) + '</div></div>'
            + '<div class="tile submitted"><div class="k">已提交</div><div class="v">' + deps.workbenchNumber(deps.workbenchCount(q.submitted)) + '</div></div>'
            + '</div>';
    }
    // 概览里每个阶段压缩成一行：阶段名 + 三个关键数字 + 完成小字。
    // 三个数字是「现在怎么样」（排队/未完成（含排队）/错误），完成量是累计视角，放小字不占视线；
    // 已提交=累计提交（≈完成+失败+在途），在概览里重复感太强，只在站点 Tab 的完整详情里出现。
    function stageLine(name, st) {
        var q = (st && st.queue) || {};
        function mini(cls, label, val) {
            return '<span class="mini ' + cls + '" title="' + label + '">' + '<i></i>' + label
                + ' <b>' + deps.workbenchNumber(deps.workbenchCount(val)) + '</b></span>';
        }
        return '<div class="stage-line">'
            + '<span class="stage-name">' + deps.esc(stageLabel(name, st)) + '</span>'
            + '<span class="mini-group">'
            + mini('queued', '排队', q.queue_len)
            + mini('running', '未完成（含排队）', q.in_progress)
            + mini('failed', '错误', q.failed)
            + '</span>'
            + '<span class="stage-done">完成 ' + deps.workbenchNumber(deps.workbenchCount(q.completed)) + '</span>'
            + '</div>';
    }
    // 站点 Tab 里的完整阶段详情：五格大数字 + 累计耗时 + Worker 明细表。
    // 这块原来是独立的「任务队列」模块，模块已并入 Dashboard 的「任务队列概览」——
    // 概览（全部）保持紧凑的站点卡片，切到某站点时才展开到这一层，信息不丢。
    function stageGlobal(st) {
        var g = (st && st.global) || {};
        return '累计任务 ' + (g.TotalTasks || 0) + ' · 累计失败 ' + (g.TotalFailed || 0)
            + ' · 平均 ' + deps.fmtDuration(g.AvgTime) + ' · 最大 ' + deps.fmtDuration(g.MaxTime)
            + ' · 最小 ' + deps.fmtDuration(g.MinTime);
    }
    // workerTable 画某阶段的 Worker 明细（池内每个 worker 的任务数/失败数/耗时快照）。
    function workerTable(st) {
        var workers = (st && st.workers) || {};
        var ids = Object.keys(workers);
        if (ids.length === 0) return '<div class="muted">暂无 Worker 明细</div>';
        var rows = ids.map(function (id) {
            var w = workers[id];
            return '<tr><td>#' + deps.esc(id) + '</td><td>' + (w.TotalTasks || 0) + '</td><td>' + (w.FailedTasks || 0)
                + '</td><td>' + deps.fmtDuration(w.TotalTime) + '</td><td>' + deps.fmtDuration(w.MaxTime)
                + '</td><td>' + deps.fmtDuration(w.MinTime) + '</td></tr>';
        }).join('');
        return '<div class="table-wrap"><table><thead><tr><th>Worker</th><th>任务</th><th>失败</th>'
            + '<th>总耗时</th><th>最大</th><th>最小</th></tr></thead><tbody>' + rows + '</tbody></table></div>';
    }
    function stageDetail(name, st) {
        return '<div class="stage"><h3>' + deps.esc(stageLabel(name, st)) + '</h3>' + stageTiles(st.queue)
            + '<div class="muted">' + stageGlobal(st) + '</div>'
            + '<div class="stage-workers"><div class="muted">Worker 明细</div>' + workerTable(st) + '</div></div>';
    }
    // 任务队列概览分两种形态：
    //   - 概览（全部站点）：**按站点分卡片**。多站都叫 catalog/detail，平铺在一起只看阶段名
    //     分不清是哪个站；卡片里每个阶段只占一行，扫一眼就知道哪站在排队/出错。
    //   - 选中某站点 Tab：显示该站的多阶段详情（五格大数字 + 累计耗时 + Worker 明细表）。
    function renderQueueSummary(stages, sites) {
        var el = deps.document.getElementById('dash-queue');
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
                var head = '<div class="site-card-head"><h3>' + deps.esc(siteLabel(site)) + '</h3>'
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
        var el = deps.document.getElementById('site-tabs');
        if (!el) return;
        var sites = (data && data.sites) || {};
        var keys = deps.buildWorkbench(data, null).rows.map(function (row) { return row.site; }).sort();
        if (currentSite !== null && keys.indexOf(currentSite) < 0) {
            currentSite = null; // 站点没了回概览
            refreshSiteModule();
        }
        if (keys.length === 0) { // 还没有站点快照（老引擎 / 没播种）：清理失效范围后整排隐藏
            el.innerHTML = '';
            siteTabsHTML.delete(el);
            el.hidden = true;
            deps.document.getElementById('site-meta').hidden = true;
            return;
        }
        var focus = el.contains(deps.document.activeElement) && deps.document.activeElement.dataset ? Object.assign({}, deps.document.activeElement.dataset) : null;
        if (focus && focus.site && keys.indexOf(focus.site) < 0) focus = {all:'true'};
        var tabsHTML = ['<button class="site-tab' + (currentSite === null ? ' active' : '') +
            '" data-all="true">概览（全部）</button>'].concat(keys.map(function (k) {
                return '<button class="site-tab' + (currentSite === k ? ' active' : '') +
                    '" data-site="' + deps.esc(k) + '">' + deps.esc(siteLabel(k)) + '</button>';
            })).join('');
        if (siteTabsHTML.get(el) !== tabsHTML) { el.innerHTML = tabsHTML; siteTabsHTML.set(el, tabsHTML); }
        // 与队列按钮同一套：data-* 带运行期值，内联属性里只有固定代码（站点名不拼进 JS 字符串）
        el.querySelectorAll('.site-tab').forEach(function (b) {
            b.onclick = function () { switchSite(b.dataset.all === 'true' ? null : b.dataset.site); };
            b.setAttribute && b.setAttribute('aria-pressed', String(b.dataset.all === 'true' ? currentSite === null : b.dataset.site === currentSite));
            if (focus && focus.all === b.dataset.all && focus.site === b.dataset.site) b.focus();
        });
        el.hidden = false;
        renderSiteMeta(sites);
        if (typeof deps.syncMonitorSiteScope === 'function') deps.syncMonitorSiteScope();
    }

    function renderSiteMeta(sites) {
        var meta = deps.document.getElementById('site-meta');
        if (!meta) return;
        var stat = currentSite === null || !Object.prototype.hasOwnProperty.call(sites, currentSite) ? null : sites[currentSite];
        if (!stat) { meta.hidden = true; meta.innerHTML = ''; return; }
        var parts = [
            '<b>' + deps.esc(siteLabel(stat.key)) + '</b>',
            deps.esc(stat.base_url || '—'),
            (stat.stage_count || 0) + ' 个阶段',
            // 这两列只由**手动暂停**写（熔断触发不写它），所以别叫"熔断闸住" ——
            // 一次后台点的暂停会被显示成熔断，读的人会去找"哪来的失败"。
            stat.breaker_paused
                ? '<b>已暂停</b>（' + deps.esc(deps.fmtAgo(stat.breaker_paused_at)) + '）'
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
        if (deps.getActiveModule() === 'tables' && deps.getCurrentTable() && (typeof deps.monitorTableSupportsSite !== 'function' || deps.monitorTableSupportsSite(deps.getCurrentTable()))) {
            deps.renderTableView(deps.getCurrentTable());
        }
    }

    var lastData = null;

    function renderAll(data) {
        lastData = data;
        renderSiteTabs(data);
        deps.renderWorkbench(data);
        if (typeof deps.renderMonitorVisuals === 'function') deps.renderMonitorVisuals(data);
        var view = siteView(data.stages, data.queues, data.sites);
        renderSystem(data.system, data.generated_at);
        renderDirs(data.system && data.system.dirs);
        renderQueueSummary(view.stages, view.sites);
        deps.renderQueues(view.queues);
        if (typeof deps.renderQueueGovernance === 'function') deps.renderQueueGovernance(data, currentSite);
        // 键名是**复数**：服务端 monitor.go 写的是 resp["breakers"]（数组，每站一条）。
        // 这里曾经写成 data.breaker，于是横幅永远拿不到数据、（list || []）把它吞成空数组，
        // 一个字都不报 —— 多站熔断横幅其实一直没显示过。
        // **不过滤**：铃铛是告警，切到哪个站点 tab 都该看得见（切走了反而看不见出事的是哪个站）。
        deps.renderBell(data.breakers);
        // 站点控制卡片：跟着站点 Tab 过滤（概览 = 全部站点），暂停状态取自同一轮的 breakers。
        deps.renderSiteControl(view.sites, data.breakers);
    }


function reset(){lastData=null;currentSite=null;['dash-queue','dash-sys','dash-dirs','dash-site-ctl','site-tabs','site-meta'].forEach(function(id){var el=deps.document.getElementById(id);if(el)el.innerHTML='';});}

function getSelectedSite(){return currentSite;} function selectMonitorSite(site){currentSite=site;} function getMonitorSnapshot(){return lastData;}

return Object.defineProperties({}, {
"renderSystem":{enumerable:true,get:function(){return renderSystem;},set:function(value){renderSystem=value;}},
"renderDirs":{enumerable:true,get:function(){return renderDirs;},set:function(value){renderDirs=value;}},
"stageTiles":{enumerable:true,get:function(){return stageTiles;},set:function(value){stageTiles=value;}},
"stageLine":{enumerable:true,get:function(){return stageLine;},set:function(value){stageLine=value;}},
"stageGlobal":{enumerable:true,get:function(){return stageGlobal;},set:function(value){stageGlobal=value;}},
"workerTable":{enumerable:true,get:function(){return workerTable;},set:function(value){workerTable=value;}},
"stageDetail":{enumerable:true,get:function(){return stageDetail;},set:function(value){stageDetail=value;}},
"renderQueueSummary":{enumerable:true,get:function(){return renderQueueSummary;},set:function(value){renderQueueSummary=value;}},
"siteLabel":{enumerable:true,get:function(){return siteLabel;},set:function(value){siteLabel=value;}},
"stageLabel":{enumerable:true,get:function(){return stageLabel;},set:function(value){stageLabel=value;}},
"siteView":{enumerable:true,get:function(){return siteView;},set:function(value){siteView=value;}},
"renderSiteTabs":{enumerable:true,get:function(){return renderSiteTabs;},set:function(value){renderSiteTabs=value;}},
"renderSiteMeta":{enumerable:true,get:function(){return renderSiteMeta;},set:function(value){renderSiteMeta=value;}},
"switchSite":{enumerable:true,get:function(){return switchSite;},set:function(value){switchSite=value;}},
"refreshSiteModule":{enumerable:true,get:function(){return refreshSiteModule;},set:function(value){refreshSiteModule=value;}},
"renderAll":{enumerable:true,get:function(){return renderAll;},set:function(value){renderAll=value;}},
"reset":{enumerable:true,get:function(){return reset;},set:function(value){reset=value;}},
"getSelectedSite":{enumerable:true,get:function(){return getSelectedSite;},set:function(value){getSelectedSite=value;}},
"selectMonitorSite":{enumerable:true,get:function(){return selectMonitorSite;},set:function(value){selectMonitorSite=value;}},
"getMonitorSnapshot":{enumerable:true,get:function(){return getMonitorSnapshot;},set:function(value){getMonitorSnapshot=value;}}
});
});
MonitorShell.install("monitoring/controller");
