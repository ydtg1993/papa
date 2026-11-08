/* Read-only queue projections. No polling, private engine access or database scans. */
'use strict';
var queueGovernanceSnapshot = null, queueGovernanceInstance = null;

function buildQueueGovernance(data, site) {
    data = data || {};
    var pauses = workbenchPauses(data);
    var rows = Object.keys(data.stages || {}).sort().map(function (key) {
        var stage = data.stages[key] || {}, q = stage.queue || {};
        var relevant = pauses.filter(function (p) { return (!p.site || p.site === stage.site) && (!p.stage || p.stage === stage.stage); });
        var known = Array.isArray(data.breakers) || !!(data.sites && data.sites[stage.site] && typeof data.sites[stage.site].breaker_paused === 'boolean');
        return { key: key, site: stage.site || '', stage: stage.stage || key, rawStage: stage.stage,
            queued: workbenchCount(q.queue_len), unfinished: workbenchCount(q.in_progress),
            submitted: workbenchCount(q.submitted), completed: workbenchCount(q.completed), failed: workbenchCount(q.failed),
            workers: stage.workers && typeof stage.workers === 'object' ? stage.workers : null,
            global: stage.global || {}, pauses: relevant, pauseKnown: known };
    }).filter(function (row) { return site === null || row.site === site; });
    return { rows: rows, queued: rows.length ? rows.reduce(function (n, r) { return workbenchAdd(n, r.queued); }, 0) : null,
        unfinished: rows.length ? rows.reduce(function (n, r) { return workbenchAdd(n, r.unfinished); }, 0) : null };
}

function queueDuration(n) { return typeof n === 'number' && Number.isFinite(n) && n >= 0 ? fmtDuration(n) : '—'; }
function queueWorkerHTML(workers) {
    if (!workers) return '<p class="hint">宿主未提供 Worker 快照。</p>';
    var keys = Object.keys(workers).sort(function (a, b) { return Number(a) - Number(b); });
    if (!keys.length) return '<p class="hint">当前没有 Worker 统计。</p>';
    return '<div class="table-wrap"><table class="qtable"><thead><tr><th>Worker</th><th>累计任务</th><th>累计失败</th><th>累计耗时</th><th>最小 / 最大耗时</th></tr></thead><tbody>' + keys.map(function (key) {
        var w = workers[key] || {};
        return '<tr><td>' + esc(key) + '</td><td>' + workbenchNumber(workbenchCount(w.TotalTasks)) + '</td><td>' + workbenchNumber(workbenchCount(w.FailedTasks)) + '</td><td>' + queueDuration(w.TotalTime) + '</td><td>' + queueDuration(w.MinTime) + ' / ' + queueDuration(w.MaxTime) + '</td></tr>';
    }).join('') + '</tbody></table></div>';
}

function queueStageHTML(row) {
    var state = row.pauses.length ? '已暂停投递' : row.pauseKnown ? '未暂停' : '暂停状态未知';
    var workers = row.workers ? Object.keys(row.workers).length : null;
    var g = row.global;
    return '<details class="queue-stage" data-queue-stage="' + esc(row.key) + '"><summary><span><strong>' + esc(row.stage) + '</strong><small>' + esc(row.site || '站点未知') + ' · ' + esc(state) + '</small></span><span>排队 <b>' + workbenchNumber(row.queued) + '</b></span><span>未完成 <b>' + workbenchNumber(row.unfinished) + '</b></span><span>完成 / 失败 <b>' + workbenchNumber(row.completed) + ' / ' + workbenchNumber(row.failed) + '</b></span></summary>'
        + '<div class="queue-stage-body"><p class="hint">累计提交 ' + workbenchNumber(row.submitted) + ' · Worker 统计数 ' + workbenchNumber(workers) + ' · 平均耗时 ' + queueDuration(g.AvgTime) + ' · 最小 / 最大 ' + queueDuration(g.MinTime) + ' / ' + queueDuration(g.MaxTime) + '</p>'
        + row.pauses.map(function (p) { return '<p class="hint queue-error">' + esc(p.source === 'breaker' ? '熔断' : '暂停') + '：' + esc(p.reason || '未提供原因') + ' · ' + esc(workbenchTime(p.paused_at) || '时间未知') + '</p>'; }).join('')
        + (row.site && row.rawStage ? '<button type="button" class="btn" data-queue-tasks data-site="' + esc(row.site) + '" data-stage="' + esc(row.rawStage) + '">查看阶段任务与 Trace</button>' : '<p class="hint">缺少站点或阶段标识，无法定位任务。</p>')
        + queueWorkerHTML(row.workers) + '</div></details>';
}

function renderQueueGovernance(data, site) {
    var el = document.getElementById('queue-stages'), summary = document.getElementById('queue-overview');
    if (!el || !summary) return;
    var sameInstance = queueGovernanceInstance === data.instance_id;
    queueGovernanceInstance = data.instance_id;
    queueGovernanceSnapshot = data.generated_at || null;
    var model = buildQueueGovernance(data, site), open = new Set();
    if (sameInstance) el.querySelectorAll('details[open]').forEach(function (item) { open.add(item.dataset.queueStage); });
    var focused = document.activeElement, focusedStage = null, taskFocus = false;
    if (el.contains(focused) && focused.closest) {
        var parent = focused.closest('[data-queue-stage]');
        if (parent) { focusedStage = parent.dataset.queueStage; taskFocus = focused.hasAttribute('data-queue-tasks'); }
    }
    summary.innerHTML = '<div><small>当前范围</small><strong>' + esc(site === null ? '全部站点' : site) + '</strong></div><div><small>已上报阶段</small><strong>' + model.rows.length + '</strong></div><div><small>内存排队</small><strong>' + workbenchNumber(model.queued) + '</strong></div><div><small>未完成（含排队）</small><strong>' + workbenchNumber(model.unfinished) + '</strong></div>';
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
            if (currentModule !== 'queues') return;
            currentSite = button.dataset.site;
            openTable('task', '采集任务', null, { site: button.dataset.site, stage: button.dataset.stage });
        };
    });
    setQueueGovernanceConnection(!workbenchConnection.failed, workbenchConnection.message);
}

function setQueueGovernanceConnection(ok, message) {
    var el = document.getElementById('queue-freshness');
    if (!el) return;
    var time = workbenchTime(queueGovernanceSnapshot);
    var stale = time && Date.now() - new Date(queueGovernanceSnapshot).getTime() > 30000;
    el.textContent = (!ok ? '连接异常：' + (message || '等待恢复') + '。' : stale ? '快照已超过 30 秒。' : '') + (time ? '快照生成于 ' + time + '；后台积压以各自采样时间为准。' : '尚未获得带时间的监控快照。');
    el.classList.toggle('queue-stale', !ok || !!stale);
}

function resetQueueGovernance() {
    queueGovernanceSnapshot = null; queueGovernanceInstance = null;
    ['queue-stages', 'queue-overview', 'queue-gov', 'queue-gov-msg'].forEach(function (id) { var el = document.getElementById(id); if (el) el.innerHTML = ''; });
    setQueueGovernanceConnection(false, '等待当前身份的快照');
}
