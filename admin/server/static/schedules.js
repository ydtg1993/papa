/* Current periodic batches. Writes reuse the audited task action API. */
'use strict';
var scheduleView = { listSeq: 0, detailSeq: 0, after: '0', history: [], next: '0', id: '', memberAfter: '0', memberHistory: [], data: null, items: [], busy: false };
var scheduleStatusNames = ['待处理', '处理中', '成功', '失败'];
function invalidateSchedules() { scheduleView.listSeq++; scheduleView.detailSeq++; }
function scheduleTime(value) { return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '尚未排期'; }
function scheduleStatus(value) { return scheduleStatusNames[value] || '未知'; }
function scheduleButton(label, action, id, disabled) {
    return '<button class="btn" type="button" data-schedule-action="' + action + '" data-id="' + esc(id) + '"' + (disabled ? ' disabled' : '') + '>' + label + '</button>';
}
function scheduleError(el, message) { el.innerHTML = '<div class="empty" role="status">' + esc(message) + '</div>'; }
function bindScheduleButtons(el) {
    el.querySelectorAll('[data-schedule-action]').forEach(function (button) {
        button.addEventListener('click', function () {
            var action = button.dataset.scheduleAction, id = button.dataset.id;
            if (action === 'detail') {
                scheduleView.id = id; scheduleView.memberAfter = '0'; scheduleView.memberHistory = [];
                document.getElementById('schedule-failed').checked = false;
                loadScheduleDetail('');
            } else scheduleTaskAction(action, id);
        });
    });
}
async function loadSchedules(reset) {
    if (reset) { scheduleView.after = '0'; scheduleView.history = []; }
    var seq = ++scheduleView.listSeq, site = currentSite, epoch = authGeneration;
    var el = document.getElementById('schedule-list');
    var current = function () { return seq === scheduleView.listSeq && currentModule === 'schedules' && site === currentSite && epoch === authGeneration; };
    scheduleView.detailSeq++; scheduleView.data = null; scheduleView.id = '';
    document.getElementById('schedule-detail').hidden = true;
    document.getElementById('schedule-list-next').disabled = true;
    document.getElementById('schedule-list-prev').disabled = true;
    scheduleError(el, '正在读取控制任务…');
    document.getElementById('schedule-scope').textContent = site === null ? '全部站点' : site;
    try {
        var resp = await apiFetch('/api/rule-schedules?after=' + encodeURIComponent(scheduleView.after) + (site === null ? '' : '&site=' + encodeURIComponent(site)), { current: current });
        if (!current()) return;
        if (!resp) { scheduleError(el, '读取失败，请点击刷新重试。'); return; }
        var data = await resp.json();
        if (!current()) return;
        scheduleView.next = data.next || '0';
        scheduleView.items = data.items;
        renderScheduleList(el);
        document.getElementById('schedule-list-prev').disabled = !scheduleView.history.length;
        document.getElementById('schedule-list-next').disabled = scheduleView.next === '0';
    } catch (e) { if (current()) scheduleError(el, '响应读取失败，请刷新重试。'); }
}
function renderScheduleList(el) {
        if (!scheduleView.items.length) scheduleError(el, '当前范围没有周期控制任务。配置并提交周期采集任务后即可在这里查看。');
        else {
            el.innerHTML = '<div class="schedule-scroll"><table class="schedule-table"><thead><tr><th>控制任务 / 站点</th><th>阶段</th><th>本次调度</th><th>周期</th><th>下次轮询</th><th>操作</th></tr></thead><tbody>' + scheduleView.items.map(function (row) {
                return '<tr><td>#' + esc(row.id) + '<br><strong>' + esc(row.site) + '</strong></td><td>' + esc(row.stage) + '</td><td>' + esc(scheduleStatus(row.status)) + '</td><td>' + (row.repeatable ? (row.repeat_interval ? esc(row.repeat_interval) + ' 秒' : '使用站点默认周期') : '已停止轮询') + '</td><td>' + esc(row.repeatable ? scheduleTime(row.next_repeat_at) : '—') + '</td><td>' + scheduleButton('查看批次', 'detail', row.id) + '</td></tr>';
            }).join('') + '</tbody></table></div>';
            bindScheduleButtons(el);
        }
}
function pageSchedules(next) {
    if (next) { scheduleView.history.push(scheduleView.after); scheduleView.after = scheduleView.next; }
    else scheduleView.after = scheduleView.history.pop() || '0';
    loadSchedules(false);
}
async function loadScheduleDetail(batch) {
    var seq = ++scheduleView.detailSeq, id = scheduleView.id, epoch = authGeneration;
    var panel = document.getElementById('schedule-detail'), body = document.getElementById('schedule-detail-body');
    panel.hidden = false;
    scheduleView.data = null;
    var current = function () { return seq === scheduleView.detailSeq && id === scheduleView.id && currentModule === 'schedules' && epoch === authGeneration; };
    scheduleError(body, '正在读取当前批次…');
    document.getElementById('schedule-member-next').disabled = true;
    document.getElementById('schedule-member-prev').disabled = true;
    try {
        var url = '/api/rule-schedules/detail?id=' + encodeURIComponent(id) + '&after=' + encodeURIComponent(scheduleView.memberAfter);
        if (batch) url += '&batch=' + encodeURIComponent(batch);
        if (document.getElementById('schedule-failed').checked) url += '&failed=1';
        var resp = await apiFetch(url, { current: current });
        if (!current()) return;
        if (!resp) { scheduleError(body, '读取失败或批次已切换，请点击「刷新当前批次」重试。'); return; }
        var data = await resp.json();
        if (!current()) return;
        scheduleView.data = data;
        scheduleView.items = scheduleView.items.map(function (row) { return String(row.id) === String(data.control.id) ? data.control : row; });
        renderScheduleList(document.getElementById('schedule-list'));
        renderScheduleDetail(body, data);
        document.getElementById('schedule-member-next').disabled = data.next === '0';
        document.getElementById('schedule-member-prev').disabled = !scheduleView.memberHistory.length;
    } catch (e) { if (current()) scheduleError(body, '响应读取失败，请刷新当前批次。'); }
}
function refreshScheduleDetail() {
    scheduleView.memberAfter = '0'; scheduleView.memberHistory = [];
    if (scheduleView.id) loadScheduleDetail('');
}
function pageScheduleMembers(next) {
    if (!scheduleView.data) return;
    var batch = scheduleView.data.batch.BatchID;
    if (next) { scheduleView.memberHistory.push(scheduleView.memberAfter); scheduleView.memberAfter = scheduleView.data.next; }
    else scheduleView.memberAfter = scheduleView.memberHistory.pop() || '0';
    loadScheduleDetail(batch);
}
function renderScheduleDetail(el, data) {
    var c = data.control, b = data.batch;
    var phase = !b.BatchID ? '尚未创建首批' : b.Failed ? '失败阻塞 · 等待原批重试' : b.Complete ? (c.repeatable ? '本批已完成 · 等待下次轮询' : '本批已完成 · 后续轮询已停止') : !b.RootID || b.RootID === '0' ? '根任务待恢复' : '采集中';
    var rules = Object.keys(data.rules).sort().map(function (stage) { var r = data.rules[stage]; return esc(stage) + ' → ' + esc(r.id) + ' @ ' + esc(r.version); }).join('<br>');
    el.innerHTML = '<div class="schedule-heading"><div><h3>控制任务 #' + esc(c.id) + ' · 第 ' + esc(b.Sequence) + ' 批</h3><p role="status">' + phase + '</p></div><div class="schedule-actions">'
        + scheduleButton(c.repeatable ? '停止后续轮询' : '恢复轮询', c.repeatable ? 'repeat_off' : 'repeat_on', c.id, scheduleView.busy)
        + scheduleButton('重试本次调度', 'retry', c.id, scheduleView.busy || c.status === 1)
        + scheduleButton('追踪控制任务', 'trace', c.id, scheduleView.busy) + '</div></div>'
        + '<dl class="schedule-facts"><dt>批次 ID</dt><dd>' + esc(b.BatchID || '—') + '</dd><dt>固定发布</dt><dd>' + esc(b.ReleaseID || '静态 Registry / 尚未创建') + (b.ReleaseID ? ' · 代次 ' + esc(b.ReleaseGeneration) : '') + '</dd><dt>固定规则</dt><dd>' + (rules || '—') + '</dd><dt>下次控制轮询</dt><dd>' + esc(c.repeatable ? scheduleTime(c.next_repeat_at) : '已停止') + '</dd></dl>'
        + '<div class="schedule-counts">' + [['全部', b.Total], ['待处理', b.Pending], ['处理中', b.Processing], ['成功', b.Success], ['失败', b.Failed]].map(function (x) { return '<div><span>' + x[0] + '</span><strong>' + esc(x[1]) + '</strong></div>'; }).join('') + '</div>'
        + '<p class="hint">停止轮询不取消已提交成员；重试调度不会强制创建新批次。失败成员须原批重试，全部成功后才推进。成员按 ID 升序，每页最多 50 条。</p>'
        + (data.members.length ? '<div class="schedule-scroll"><table class="schedule-table"><thead><tr><th>成员任务</th><th>阶段</th><th>状态</th><th>操作</th></tr></thead><tbody>' + data.members.map(function (row) {
            return '<tr><td>#' + esc(row.id) + (String(row.id) === String(b.RootID) ? ' · 根任务' : '') + '</td><td>' + esc(row.stage) + '</td><td>' + esc(scheduleStatus(row.status)) + '</td><td><div class="schedule-actions">' + scheduleButton('追踪 / 证据', 'trace', row.id, scheduleView.busy) + (row.status === 3 && !row.repeatable ? scheduleButton('原批重试', 'retry', row.id, scheduleView.busy) : '') + (row.repeatable ? '<span class="hint">请先到任务表关闭成员轮询</span>' : '') + '</div></td></tr>';
        }).join('') + '</tbody></table></div>' : '<div class="empty">当前筛选没有成员任务</div>');
    bindScheduleButtons(el);
}
async function scheduleTaskAction(action, id) {
    if (scheduleView.busy || !scheduleView.data) return;
    var snapshot = scheduleView.data, epoch = authGeneration, viewID = scheduleView.id;
    var row = String(snapshot.control.id) === id ? snapshot.control : snapshot.members.find(function (member) { return String(member.id) === id; });
    if (!row || ['retry', 'repeat_on', 'repeat_off', 'trace'].indexOf(action) < 0) return;
    scheduleView.busy = true;
    renderScheduleDetail(document.getElementById('schedule-detail-body'), snapshot);
    try {
        if (action !== 'trace') {
            var message = action === 'retry' ? '重试当前任务，保留原批次与固定规则。此操作不会强制开始新批次。' : action === 'repeat_off' ? '停止后续控制轮询，已提交的成员会继续运行。' : '恢复控制轮询，未完成或失败的批次仍会阻止推进。';
            if (!await Dialog.confirm({ title: '控制任务 / 成员 #' + id, body: esc(message) })) return;
        }
        if (epoch !== authGeneration || currentModule !== 'schedules' || scheduleView.data !== snapshot) return;
        var resp = await apiPost('/api/oao/task/action/' + action, { id: String(id), row: { reprocess: row.reprocess }, values: {} });
        if (resp && action !== 'trace') {
            Toast.ok('任务 #' + id + ' 操作已受理');
            if (currentModule === 'schedules' && scheduleView.id === viewID) refreshScheduleDetail();
        }
    } finally {
        scheduleView.busy = false;
        if (currentModule === 'schedules' && scheduleView.data) renderScheduleDetail(document.getElementById('schedule-detail-body'), scheduleView.data);
        // Rendering replaces the original button; restore keyboard focus only
        // if the user is still here and no newer dialog/input has taken focus.
        if (epoch === authGeneration && currentModule === 'schedules' && scheduleView.id === viewID && document.activeElement === document.body) {
            document.getElementById('schedule-detail-body').querySelectorAll('[data-schedule-action]').forEach(function (button) {
                if (button.dataset.scheduleAction === action && button.dataset.id === id && !button.disabled) button.focus();
            });
        }
    }
}
