/* queues/queue-governance: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("queues/queue-governance","queues",["document","esc","fmtDuration","getActiveModule","getWorkbenchConnection","openTable","selectMonitorSite","workbenchAdd","workbenchCount","workbenchNumber","workbenchPauses","workbenchTime"],function(deps,lifecycle){
'use strict';
/* Read-only queue projections. No polling, private engine access or database scans. */
'use strict';
var queueGovernanceSnapshot = null, queueGovernanceInstance = null;

function buildQueueGovernance(data, site) {
    data = data || {};
    var pauses = deps.workbenchPauses(data);
    var rows = Object.keys(data.stages || {}).sort().map(function (key) {
        var stage = data.stages[key] || {}, q = stage.queue || {};
        var relevant = pauses.filter(function (p) { return (!p.site || p.site === stage.site) && (!p.stage || p.stage === stage.stage); });
        var known = Array.isArray(data.breakers) || !!(data.sites && data.sites[stage.site] && typeof data.sites[stage.site].breaker_paused === 'boolean');
        return { key: key, site: stage.site || '', stage: stage.stage || key, rawStage: stage.stage,
            queued: deps.workbenchCount(q.queue_len), unfinished: deps.workbenchCount(q.in_progress),
            submitted: deps.workbenchCount(q.submitted), completed: deps.workbenchCount(q.completed), failed: deps.workbenchCount(q.failed),
            workers: stage.workers && typeof stage.workers === 'object' ? stage.workers : null,
            global: stage.global || {}, pauses: relevant, pauseKnown: known };
    }).filter(function (row) { return site === null || row.site === site; });
    return { rows: rows, queued: rows.length ? rows.reduce(function (n, r) { return deps.workbenchAdd(n, r.queued); }, 0) : null,
        unfinished: rows.length ? rows.reduce(function (n, r) { return deps.workbenchAdd(n, r.unfinished); }, 0) : null };
}

function queueDuration(n) { return typeof n === 'number' && Number.isFinite(n) && n >= 0 ? deps.fmtDuration(n) : '—'; }
function queueWorkerHTML(workers) {
    if (!workers) return '<p class="hint">宿主未提供 Worker 快照。</p>';
    var keys = Object.keys(workers).sort(function (a, b) { return Number(a) - Number(b); });
    if (!keys.length) return '<p class="hint">当前没有 Worker 统计。</p>';
    return '<div class="table-wrap"><table class="qtable"><thead><tr><th>Worker</th><th>累计任务</th><th>累计失败</th><th>累计耗时</th><th>最小 / 最大耗时</th></tr></thead><tbody>' + keys.map(function (key) {
        var w = workers[key] || {};
        return '<tr><td>' + deps.esc(key) + '</td><td>' + deps.workbenchNumber(deps.workbenchCount(w.TotalTasks)) + '</td><td>' + deps.workbenchNumber(deps.workbenchCount(w.FailedTasks)) + '</td><td>' + queueDuration(w.TotalTime) + '</td><td>' + queueDuration(w.MinTime) + ' / ' + queueDuration(w.MaxTime) + '</td></tr>';
    }).join('') + '</tbody></table></div>';
}

function queueStageHTML(row) {
    var state = row.pauses.length ? '已暂停投递' : row.pauseKnown ? '未暂停' : '暂停状态未知';
    var workers = row.workers ? Object.keys(row.workers).length : null;
    var g = row.global;
    return '<details class="queue-stage" data-queue-stage="' + deps.esc(row.key) + '"><summary><span><strong>' + deps.esc(row.stage) + '</strong><small>' + deps.esc(row.site || '站点未知') + ' · ' + deps.esc(state) + '</small></span><span>排队 <b>' + deps.workbenchNumber(row.queued) + '</b></span><span>未完成 <b>' + deps.workbenchNumber(row.unfinished) + '</b></span><span>完成 / 失败 <b>' + deps.workbenchNumber(row.completed) + ' / ' + deps.workbenchNumber(row.failed) + '</b></span></summary>'
        + '<div class="queue-stage-body"><p class="hint">累计提交 ' + deps.workbenchNumber(row.submitted) + ' · Worker 统计数 ' + deps.workbenchNumber(workers) + ' · 平均耗时 ' + queueDuration(g.AvgTime) + ' · 最小 / 最大 ' + queueDuration(g.MinTime) + ' / ' + queueDuration(g.MaxTime) + '</p>'
        + row.pauses.map(function (p) { return '<p class="hint queue-error">' + deps.esc(p.source === 'breaker' ? '熔断' : '暂停') + '：' + deps.esc(p.reason || '未提供原因') + ' · ' + deps.esc(deps.workbenchTime(p.paused_at) || '时间未知') + '</p>'; }).join('')
        + (row.site && row.rawStage ? '<button type="button" class="btn" data-queue-tasks data-site="' + deps.esc(row.site) + '" data-stage="' + deps.esc(row.rawStage) + '">查看阶段任务与 Trace</button>' : '<p class="hint">缺少站点或阶段标识，无法定位任务。</p>')
        + queueWorkerHTML(row.workers) + '</div></details>';
}

function renderQueueGovernance(data, site) {
    var el = deps.document.getElementById('queue-stages'), summary = deps.document.getElementById('queue-overview');
    if (!el || !summary) return;
    var sameInstance = queueGovernanceInstance === data.instance_id;
    queueGovernanceInstance = data.instance_id;
    queueGovernanceSnapshot = data.generated_at || null;
    var model = buildQueueGovernance(data, site), open = new Set();
    if (sameInstance) el.querySelectorAll('details[open]').forEach(function (item) { open.add(item.dataset.queueStage); });
    var focused = deps.document.activeElement, focusedStage = null, taskFocus = false;
    if (el.contains(focused) && focused.closest) {
        var parent = focused.closest('[data-queue-stage]');
        if (parent) { focusedStage = parent.dataset.queueStage; taskFocus = focused.hasAttribute('data-queue-tasks'); }
    }
    summary.innerHTML = '<div><small>当前范围</small><strong>' + deps.esc(site === null ? '全部站点' : site) + '</strong></div><div><small>已上报阶段</small><strong>' + model.rows.length + '</strong></div><div><small>内存排队</small><strong>' + deps.workbenchNumber(model.queued) + '</strong></div><div><small>未完成（含排队）</small><strong>' + deps.workbenchNumber(model.unfinished) + '</strong></div>';
    el.innerHTML = model.rows.length ? model.rows.map(queueStageHTML).join('') : '<div class="empty">当前范围没有阶段队列快照。未注册、已释放或宿主未上报的队列不在此列表中；这不代表数据库没有待处理任务。</div>';
    el.querySelectorAll('[data-queue-stage]').forEach(function (item) {
        item.open = open.has(item.dataset.queueStage);
        if (item.dataset.queueStage === focusedStage) {
            var target = item.querySelector(taskFocus ? '[data-queue-tasks]' : 'summary');
            if (target) target.focus({ preventScroll: true });
        }
    });
    el.querySelectorAll('[data-queue-tasks]').forEach(function (button) {
        button.onclick = function () {
            if (deps.getActiveModule() !== 'queues') return;
            deps.selectMonitorSite(button.dataset.site);
            deps.openTable('task', '采集任务', null, { site: button.dataset.site, stage: button.dataset.stage });
        };
    });
    setQueueGovernanceConnection(!deps.getWorkbenchConnection().failed, deps.getWorkbenchConnection().message);
}

function setQueueGovernanceConnection(ok, message) {
    var el = deps.document.getElementById('queue-freshness');
    if (!el) return;
    var time = deps.workbenchTime(queueGovernanceSnapshot);
    var stale = time && Date.now() - new Date(queueGovernanceSnapshot).getTime() > 30000;
    el.textContent = (!ok ? '连接异常：' + (message || '等待恢复') + '。' : stale ? '快照已超过 30 秒。' : '') + (time ? '快照生成于 ' + time + '；后台积压以各自采样时间为准。' : '尚未获得带时间的监控快照。');
    el.classList.toggle('queue-stale', !ok || !!stale);
}

function resetQueueGovernance() {
    queueGovernanceSnapshot = null; queueGovernanceInstance = null;
    ['queue-stages', 'queue-overview', 'queue-gov', 'queue-gov-msg'].forEach(function (id) { var el = deps.document.getElementById(id); if (el) el.innerHTML = ''; });
    setQueueGovernanceConnection(false, '等待当前身份的快照');
}

function reset(){resetQueueGovernance();}



return Object.defineProperties({}, {
"buildQueueGovernance":{enumerable:true,get:function(){return buildQueueGovernance;},set:function(value){buildQueueGovernance=value;}},
"queueDuration":{enumerable:true,get:function(){return queueDuration;},set:function(value){queueDuration=value;}},
"queueWorkerHTML":{enumerable:true,get:function(){return queueWorkerHTML;},set:function(value){queueWorkerHTML=value;}},
"queueStageHTML":{enumerable:true,get:function(){return queueStageHTML;},set:function(value){queueStageHTML=value;}},
"renderQueueGovernance":{enumerable:true,get:function(){return renderQueueGovernance;},set:function(value){renderQueueGovernance=value;}},
"setQueueGovernanceConnection":{enumerable:true,get:function(){return setQueueGovernanceConnection;},set:function(value){setQueueGovernanceConnection=value;}},
"resetQueueGovernance":{enumerable:true,get:function(){return resetQueueGovernance;},set:function(value){resetQueueGovernance=value;}},
"reset":{enumerable:true,get:function(){return reset;},set:function(value){reset=value;}}
});
});
MonitorShell.install("queues/queue-governance");
