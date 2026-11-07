/* Dashboard chart assembly; all data comes from accepted monitor snapshots. */
'use strict';
var monitorVisuals = { instances: new Map(), data: null, failed: false, active: false, suspended: false, disclosures: new WeakSet() };
var monitorAnalysisCharts = ['visual-stage-bars', 'visual-results'];
var monitorResourceCharts = ['visual-cpu', 'visual-mem', 'visual-disk'];
function monitorChartsAvailable() { return typeof Oao !== 'undefined' && Oao.charts && typeof Oao.charts.create === 'function'; }
function destroyMonitorChartGroup(ids) {
    ids.forEach(function (id) {
        var chart = monitorVisuals.instances.get(id);
        if (chart) { chart.destroy(); monitorVisuals.instances.delete(id); }
    });
}
function cancelMonitorResourceMotion() {
    if (typeof Oao === 'undefined' || !Oao.motion || typeof Oao.motion.cancel !== 'function') return;
    ['cpu', 'mem', 'disk'].forEach(function (key) {
        var el = document.getElementById('visual-' + key + '-value');
        if (el) Oao.motion.cancel(el);
    });
}
function destroyMonitorVisuals() {
    monitorVisuals.active = false;
    destroyMonitorChartGroup(Array.from(monitorVisuals.instances.keys()));
    cancelMonitorResourceMotion();
    if (typeof cancelWorkbenchMotion === 'function') cancelWorkbenchMotion();
}
function resumeMonitorVisuals() {
    // The polling owner checks the stored credential before calling this on
    // pageshow. A reset during that check has already cleared the old data.
    monitorVisuals.suspended = false;
    if (monitorVisuals.data) renderMonitorVisuals(monitorVisuals.data);
}
function monitorDisclosureOpen(id) {
    var el = document.getElementById(id);
    return !!(el && el.open);
}
function bindMonitorDisclosures() {
    ['workbench-analysis', 'workbench-resources'].forEach(function (id) {
        var el = document.getElementById(id);
        if (!el || monitorVisuals.disclosures.has(el)) return;
        monitorVisuals.disclosures.add(el);
        el.addEventListener('toggle', function () {
            if (!el.open) {
                destroyMonitorChartGroup(id === 'workbench-analysis' ? monitorAnalysisCharts : monitorResourceCharts);
                if (id === 'workbench-resources') cancelMonitorResourceMotion();
            }
            // A queued toggle can arrive after navigation or pagehide. Only an
            // actual dashboard render may resume the chart lifecycle.
            if (!monitorVisuals.active || (typeof currentModule !== 'undefined' && currentModule !== 'dashboard')) return;
            renderMonitorVisuals(monitorVisuals.data);
        });
    });
}
function resetMonitorObservations() {
    MonitorObservations.reset(); monitorVisuals.data = null; monitorVisuals.failed = false;
    destroyMonitorVisuals();
    var meta = document.getElementById('visual-observation-meta');
    if (meta) meta.textContent = '等待新的监控快照';
    ['cpu','mem','disk'].forEach(function (key) {
        var value = document.getElementById('visual-'+key+'-value'), stamp = document.getElementById('visual-'+key+'-stamp');
        if (value) value.textContent = '—';
        if (stamp) stamp.textContent = '尚无成功采样';
    });
}
function acceptMonitorObservation(data) { monitorVisuals.data = data; monitorVisuals.failed = false; return MonitorObservations.accept(data); }
function setMonitorVisualConnection(ok) { monitorVisuals.failed = !ok; renderMonitorVisuals(monitorVisuals.data); }
function showMonitorChart(id, options) {
    var el = document.getElementById(id);
    if (!el || !monitorChartsAvailable()) return;
    var chart = monitorVisuals.instances.get(id);
    if (chart) chart.update(options);
    else monitorVisuals.instances.set(id, Oao.charts.create(el, options));
}
function renderMonitorVisuals(data) {
    if (monitorVisuals.suspended) return;
    bindMonitorDisclosures();
    if (typeof currentModule !== 'undefined' && currentModule !== 'dashboard') { destroyMonitorVisuals(); return; }
    monitorVisuals.active = true;
    if (document.hidden) { cancelMonitorResourceMotion(); return; }
    var note = document.getElementById('visual-availability'), meta = document.getElementById('visual-observation-meta');
    if (!note || !meta) return;
    var available = monitorChartsAvailable();
    note.hidden = !!available;
    var dashboard = document.getElementById('mod-dashboard');
    if (dashboard) dashboard.querySelectorAll('.visual-grid,.visual-resources').forEach(function (el) { el.hidden = !available; });
    note.textContent = available ? '' : '当前 oao 版本未包含图表扩展。监控快照仍可查看；升级 oao 后启用图表。';
    if (!available || !data) {
        destroyMonitorChartGroup(Array.from(monitorVisuals.instances.keys()));
        cancelMonitorResourceMotion();
        return;
    }
    var latest = MonitorObservations.latest(), scope = currentSite === null ? '全部站点' : currentSite;
    meta.textContent = (monitorVisuals.failed ? '连接异常，保留上次观测 · ' : '') + scope + ' · ' + MonitorObservations.size() + ' 个快照 · 最近 10 分钟 / 最多 240 个 · 刷新页面后重新观察';
    var missing = latest ? null : '此服务未提供观测时间与实例标识，无法建立可信趋势';
    showMonitorChart('visual-queue-trend', {type:'line',title:'本次页面观测',unit:'项',xType:'time',error:missing,
        description:'未完成包含排队；两条曲线不能相加。断线或阶段集合变化处留空。',
        series:[{key:'queued',label:'排队',points:MonitorObservations.trend(currentSite,'queued')},{key:'unfinished',label:'未完成（含排队）',points:MonitorObservations.trend(currentSite,'unfinished')}]});
    if (monitorDisclosureOpen('workbench-analysis')) renderMonitorAnalysis(data);
    else destroyMonitorChartGroup(monitorAnalysisCharts);
    if (monitorDisclosureOpen('workbench-resources')) renderMonitorResources(data);
    else { destroyMonitorChartGroup(monitorResourceCharts); cancelMonitorResourceMotion(); }
}
function renderMonitorAnalysis(data) {
    var stages = Object.keys(data.stages || {}).filter(function (key) { return currentSite === null || data.stages[key].site === currentSite; });
    var bars = stages.map(function (key) { var s=data.stages[key],q=s.queue||{};return {key:key,label:(currentSite===null ? s.site+' / ' : '')+(s.stage||key),value:workbenchCount(q.queue_len)}; });
    var invalid = bars.some(function (b) { return b.value === null; });
    bars.sort(function (a,b) { return b.value-a.value || a.key.localeCompare(b.key); });
    var barDescription = '活跃阶段内存队列当前快照';
    if (!invalid && bars.length > 12) {
        var remainder = bars.slice(11).reduce(function (sum,b) { return workbenchAdd(sum,b.value); },0);
        invalid = remainder === null;
        var otherKey = '__papa_other__';
        while (bars.some(function (b) { return b.key === otherKey; })) otherKey += '_';
        bars = bars.slice(0,11).concat([{key:otherKey,label:'其他 '+(stages.length-11)+' 个阶段',value:remainder}]);
        barDescription += ' · 展示前 11 项，其余合并，保留总数';
    }
    showMonitorChart('visual-stage-bars', {type:'bar',horizontal:true,title:'排队分布',unit:'项',description:barDescription,
        error:invalid?'阶段统计不完整，无法绘制分布':null,data:invalid?[]:bars,emptyText:'当前范围没有活跃阶段'});
    var totals=buildWorkbench(data,currentSite).totals;
    showMonitorChart('visual-results', {type:'donut',title:'活跃阶段已结束结果',unit:'次',description:'当前活跃阶段的累计完成 / 失败；不代表全站历史成功率。',
        error:totals.completed===null||totals.failed===null?'结果统计不完整':null,
        data:stages.length?[{key:'completed',label:'完成',value:totals.completed},{key:'failed',label:'失败',value:totals.failed}]:[],emptyText:'暂无已结束结果'});
}
function renderMonitorResources(data) {
    var now=MonitorObservations.time(data.generated_at);
    ['cpu','mem','disk'].forEach(function (key) {
        var resource=MonitorObservations.resource(data.system,key,now), el=document.getElementById('visual-'+key+'-value'), stamp=document.getElementById('visual-'+key+'-stamp');
        var value=resource.value===null?'—':resource.value.toFixed(1)+'%';
        if (el && el.textContent!==value) {el.textContent=value;if (typeof Oao!=='undefined'&&Oao.motion&&typeof Oao.motion.animate==='function') Oao.motion.animate(el);}
        if (stamp) stamp.textContent=(monitorVisuals.failed?'连接异常 · ':resource.state+' · ')+(resource.at===null?'尚无成功采样':new Date(resource.at).toLocaleTimeString('zh-CN',{hour12:false})+' 采样');
        showMonitorChart('visual-'+key, {type:'sparkline',title:({cpu:'CPU · 系统采样',mem:'内存 · 系统采样',disk:'磁盘 · 根文件系统'})[key],unit:'%',xType:'time',
            series:[{key:key,label:'使用率',points:MonitorObservations.resourceTrend(key)}],emptyText:'等待可用资源采样'});
    });
}
window.addEventListener('pagehide', function () { monitorVisuals.suspended = true; destroyMonitorVisuals(); });
document.addEventListener('visibilitychange', function () {
    if (document.hidden) cancelMonitorResourceMotion();
    else if (monitorVisuals.active && (typeof currentModule === 'undefined' || currentModule === 'dashboard')) renderMonitorVisuals(monitorVisuals.data);
});
