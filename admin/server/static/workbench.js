/* Operations overview: read-only projections of the existing monitor snapshot. */
'use strict';
var workbenchConnection = { updatedAt: null, failed: false, message: '' };
var workbenchMetrics = ['queued', 'running', 'completed', 'failed'];
var workbenchHTML = new WeakMap();
var workbenchKPIValues = [];

function workbenchCount(value) { return Number.isSafeInteger(value) && value >= 0 ? value : null; }
function workbenchAdd(a, b) { return a === null || b === null ? null : workbenchCount(a + b); }
function workbenchTime(value) {
    var d = new Date(value);
    return value && !isNaN(d.getTime()) && d.getFullYear() >= 2000 ? d.toLocaleString('zh-CN', { hour12: false }) : null;
}
function workbenchNumber(value) { return value === null ? '—' : value.toLocaleString('zh-CN'); }
function workbenchPauses(data) {
    var sites = data.sites || {};
    return Array.isArray(data.breakers) ? data.breakers.filter(function (pause) { return pause && pause.paused; }) : Object.keys(sites).filter(function (key) {
        return sites[key] && sites[key].breaker_paused;
    }).map(function (key) {
        var site = sites[key];
        return { site: key, paused: true, source: site.pause_source, reason: site.pause_reason, paused_at: site.breaker_paused_at };
    });
}

function buildWorkbench(data, site) {
    data = data || {};
    var rows = new Map(), sites = data.sites || {}, explicitPauses = Array.isArray(data.breakers);
    function rowFor(key) {
        if (typeof key !== 'string' || !key) return null;
        if (!rows.has(key)) rows.set(key, { site: key, activeStages: 0, queued: 0, running: 0, completed: 0, failed: 0, paused: false, pauseKnown: explicitPauses, tone: 'plain' });
        return rows.get(key);
    }
    Object.keys(sites).forEach(function (key) {
        var row = rowFor(key), s = sites[key] || {};
        if (row) row.pauseKnown = explicitPauses || typeof s.breaker_paused === 'boolean';
    });
    Object.keys(data.stages || {}).forEach(function (key) {
        var stage = data.stages[key] || {}, row = rowFor(stage.site), q = stage.queue || {};
        if (!row) return;
        row.activeStages++;
        [q.queue_len, q.in_progress, q.completed, q.failed].forEach(function (value, index) {
            var field = workbenchMetrics[index];
            row[field] = workbenchAdd(row[field], workbenchCount(value));
        });
    });
    var pauses = workbenchPauses(data);
    pauses.forEach(function (pause) { rowFor(pause.site); });
    var issues = [];
    pauses.forEach(function (pause) {
        rows.forEach(function (row) {
            if (!pause.site || pause.site === row.site) {
                row.paused = true; row.pauseKnown = true;
                if (pause.source === 'breaker' || row.tone !== 'danger') row.tone = pause.source === 'breaker' ? 'danger' : 'warning';
            }
        });
        if (site !== null && pause.site && pause.site !== site) return;
        issues.push({ key: JSON.stringify(['pause', pause.site || '', pause.stage || '']), kind: 'pause', priority: pause.source === 'breaker' ? 4 : 2, tone: pause.source === 'breaker' ? 'danger' : 'warning',
            title: pause.source === 'breaker' ? '熔断暂停' : '已暂停', scope: pause.site || '全部站点', site: pause.site || null, action: pause.site ? 'site' : 'pauses',
            detail: (pause.stage ? '阶段 ' + pause.stage + ' · ' : '') + (pause.reason || '请核对暂停原因后再恢复') + (workbenchTime(pause.paused_at) ? ' · 暂停于 ' + workbenchTime(pause.paused_at) : '') });
    });
    var queueNames = { error_queue: '错误队列', repeat_queue: '轮询队列' };
    Object.keys(queueNames).forEach(function (name) {
        var q = (data.queues || {})[name];
        if (!q) return;
        function issue(title, detail, priority) { issues.push({ key: name + (priority === 3 ? ':error' : ':backlog'), title: title, detail: detail, priority: priority, tone: priority >= 3 ? 'danger' : 'notice', site: null, scope: '全部站点', action: 'queues' }); }
        if (q.last_error) issue(queueNames[name] + ' · 上次执行报错', String(q.last_error) + (workbenchTime(q.last_finish_at) ? ' · 完成于 ' + workbenchTime(q.last_finish_at) : ''), 3);
        var sample = workbenchTime(q.backlog_at), backlog = workbenchCount(q.backlog);
        if (!sample) issue(queueNames[name] + ' · 积压未知', '尚未采样，不能据此判断当前积压。', 0);
        else if (backlog === null) issue(queueNames[name] + ' · 积压未知', '样本数值不可用 · 采样于 ' + sample, 0);
        else if (backlog > 0) issue(queueNames[name] + ' · 采样待处理 ' + workbenchNumber(backlog), (q.enabled ? '自动轮询已启用' : '自动轮询已停用') + ' · 采样于 ' + sample + '，非实时存量。', 1);
    });
    if (!data.queues) issues.push({ title: '治理队列监控未提供', detail: '此快照未包含治理队列统计，无法判断积压。', priority: 0, tone: 'notice', scope: '全部站点', site: null, action: null });
    var visible = Array.from(rows.values()).filter(function (row) { return site === null || row.site === site; });
    visible.forEach(function (row) {
        row.state = row.paused ? (row.tone === 'danger' ? '熔断暂停' : '已暂停') : !row.pauseKnown ? '暂停状态未知' : !row.activeStages ? '暂无活跃阶段' : row.running === null || row.queued === null ? '统计不完整' : row.running > 0 || row.queued > 0 ? '有未完成任务' : '空闲';
    });
    visible.sort(function (a, b) { return Number(b.paused) - Number(a.paused) || a.site.localeCompare(b.site); });
    var totals = { sites: visible.length, activeStages: 0, queued: 0, running: 0, completed: 0, failed: 0 };
    visible.forEach(function (row) {
        totals.activeStages += row.activeStages;
        workbenchMetrics.forEach(function (field) { totals[field] = workbenchAdd(totals[field], row[field]); });
    });
    issues.sort(function (a, b) { return b.priority - a.priority; });
    return { rows: visible, totals: totals, issues: issues };
}

function replaceWorkbenchHTML(el, html) {
    if (!el || workbenchHTML.get(el) === html) return false;
    var focused = el.contains(document.activeElement) ? document.activeElement : null;
    var target = focused && focused.dataset ? { action: focused.dataset.workbenchAction, site: focused.dataset.site, key: focused.dataset.workbenchKey } : null;
    var scroll = Array.from(el.querySelectorAll('.workbench-scroll')).map(function (region) { return { left: region.scrollLeft, top: region.scrollTop }; });
    // Replaced KPI nodes must retire their animations before leaving the DOM.
    if (typeof Oao !== 'undefined' && Oao.motion && typeof Oao.motion.cancel === 'function') el.querySelectorAll('strong').forEach(function (value) { Oao.motion.cancel(value); });
    el.innerHTML = html;
    workbenchHTML.set(el, html);
    el.querySelectorAll('.workbench-scroll').forEach(function (region, index) {
        if (scroll[index]) { region.scrollLeft = scroll[index].left; region.scrollTop = scroll[index].top; }
    });
    if (target && target.action) el.querySelectorAll('[data-workbench-action]').forEach(function (button) {
        if (button.dataset.workbenchAction === target.action && button.dataset.site === target.site && button.dataset.workbenchKey === target.key) button.focus({ preventScroll: true });
    });
    return true;
}
function cancelWorkbenchMotion() {
    var root = document.getElementById('workbench-kpis');
    if (root && typeof Oao !== 'undefined' && Oao.motion && typeof Oao.motion.cancel === 'function') root.querySelectorAll('strong').forEach(function (el) { Oao.motion.cancel(el); });
}
function workbenchButton(label, action, site, key) {
    return '<button type="button" class="btn" data-workbench-action="' + action + '" data-workbench-key="' + esc(key || action) + '"' + (site === null ? '' : ' data-site="' + esc(site) + '"') + '>' + label + '</button>';
}
function bindWorkbench(el) {
    el.querySelectorAll('[data-workbench-action]').forEach(function (button) {
        button.addEventListener('click', function (event) { navigateWorkbench(button.dataset.workbenchAction, button.dataset.site, event); });
    });
}
function navigateWorkbench(action, site, event) {
    if (currentModule !== 'dashboard') return;
    if (action === 'queues') { switchModule('queues'); return; }
    if (action === 'pauses') {
        var panel = document.getElementById('bell-panel'), bell = document.getElementById('bell');
        if (!panel || !bell || typeof toggleBell !== 'function') return;
        // This button is outside bell-wrap; do not let the document's outside
        // click handler close the panel during the same click.
        if (event) event.stopPropagation();
        if (typeof renderBell === 'function') renderBell(workbenchPauses(lastData || {}));
        if (panel.hidden) toggleBell();
        bell.focus();
        return;
    }
    if (['site', 'tasks'].indexOf(action) < 0 || !buildWorkbench(lastData, null).rows.some(function (row) { return row.site === site; })) return;
    switchSite(site);
    if (action === 'tasks') openTable('task', '任务');
    else {
        var detail = document.getElementById('workbench-stage-details');
        detail.open = true;
        detail.scrollIntoView({ block: 'start' });
        detail.querySelector('summary').focus();
    }
}
function setWorkbenchConnection(ok, message) {
    workbenchConnection.failed = !ok;
    workbenchConnection.message = message || '';
    if (ok) workbenchConnection.updatedAt = new Date().toISOString();
    renderWorkbenchFreshness();
}
function renderWorkbenchFreshness() {
    var el = document.getElementById('workbench-freshness');
    if (!el) return;
    var at = workbenchTime(workbenchConnection.updatedAt);
    el.classList.toggle('is-stale', workbenchConnection.failed);
    el.textContent = at ? (workbenchConnection.failed ? '保留上次快照 · ' + workbenchConnection.message + ' · ' : '') + '上次成功刷新 ' + at : '尚未获得监控快照' + (workbenchConnection.failed ? ' · ' + workbenchConnection.message : '');
}
function renderWorkbench(data) {
    var model = buildWorkbench(data, currentSite), totals = model.totals;
    document.getElementById('workbench-scope').textContent = currentSite === null ? '全部站点' : currentSite;
    document.getElementById('workbench-coverage').textContent = totals.sites + ' 个站点 · ' + totals.activeStages + ' 个活跃阶段';
    var cards = [['排队中', totals.queued, '活跃阶段内存队列'], ['未完成（含排队）', totals.running, '包含上项排队，不可相加'], ['累计完成', totals.completed, '活跃阶段累计'], ['累计失败', totals.failed, '累计次数，非当前失败存量']];
    var kpiRoot = document.getElementById('workbench-kpis');
    var kpiChanged = replaceWorkbenchHTML(kpiRoot, cards.map(function (card) {
        return '<div class="workbench-kpi"><span>' + card[0] + '</span><strong>' + workbenchNumber(card[1]) + '</strong><small>' + card[2] + '</small></div>';
    }).join(''));
    var canAnimate = currentModule === 'dashboard' && !document.hidden && (typeof monitorVisuals === 'undefined' || !monitorVisuals.suspended);
    if (kpiChanged && canAnimate && typeof Oao !== 'undefined' && Oao.motion && typeof Oao.motion.animate === 'function') kpiRoot.querySelectorAll('strong').forEach(function (el,index) {
        if (workbenchKPIValues[index] !== cards[index][1]) Oao.motion.animate(el);
    });
    workbenchKPIValues = cards.map(function (card) { return card[1]; });
    var issueRoot = document.getElementById('workbench-issues');
    document.getElementById('workbench-issue-count').textContent = model.issues.length;
    function issueHTML(issue) {
        return '<article class="workbench-issue" data-tone="' + issue.tone + '"><div><span class="workbench-scope">' + esc(issue.scope) + '</span><h3>' + esc(issue.title) + '</h3><p>' + esc(issue.detail) + '</p></div>'
            + (issue.action ? workbenchButton(issue.action === 'queues' ? '查看队列' : issue.action === 'pauses' ? '查看暂停详情' : '查看站点', issue.action, issue.site, issue.key) : '') + '</article>';
    }
    var content = model.issues.slice(0, 3).map(issueHTML).join(''), remaining = model.issues.slice(3);
    if (remaining.length) {
        content += '<div class="workbench-more"><p>另有 ' + remaining.length + ' 项提示';
        // A missing monitor field has no destination that can explain it.
        // Keep that uncertainty visible even when the summary is full.
        remaining.filter(function (issue) { return !issue.action; }).forEach(function (issue) { content += ' · ' + esc(issue.title) + '：' + esc(issue.detail); });
        content += '</p><div class="workbench-links">';
        if (remaining.some(function (issue) { return issue.action === 'queues'; })) content += workbenchButton('查看队列异常', 'queues', null, 'more-queues');
        if (remaining.some(function (issue) { return issue.kind === 'pause'; })) content += workbenchButton('查看暂停详情', 'pauses', null, 'more-pauses');
        content += '</div></div>';
    }
    if (!content) content = '<p class="workbench-empty">此快照未报告暂停或队列问题。累计失败不作为当前故障；任务明细请到任务表查看。</p>';
    if (replaceWorkbenchHTML(issueRoot, content)) bindWorkbench(issueRoot);
    var siteRoot = document.getElementById('workbench-sites');
    content = model.rows.length ? '<div class="workbench-scroll"><table class="workbench-table"><thead><tr><th>站点 / 阶段</th><th>状态</th><th>排队 / 未完成（含排队）</th><th>累计完成 / 失败</th><th>定位</th></tr></thead><tbody>' + model.rows.map(function (row) {
        return '<tr><td><strong>' + esc(row.site) + '</strong><small>' + row.activeStages + ' 个活跃阶段</small></td><td><span class="workbench-state" data-tone="' + row.tone + '">' + esc(row.state) + '</span></td><td>' + workbenchNumber(row.queued) + ' / ' + workbenchNumber(row.running) + '</td><td>' + workbenchNumber(row.completed) + ' / ' + workbenchNumber(row.failed) + '</td><td><div class="workbench-links">' + workbenchButton('阶段', 'site', row.site) + workbenchButton('任务', 'tasks', row.site) + '</div></td></tr>';
    }).join('') + '</tbody></table></div>' : '<p class="workbench-empty">当前范围没有本进程采集阶段的快照。仅管理模式与独立 CLI 不共享内存队列。</p>';
    if (replaceWorkbenchHTML(siteRoot, content)) bindWorkbench(siteRoot);
    renderWorkbenchFreshness();
}
