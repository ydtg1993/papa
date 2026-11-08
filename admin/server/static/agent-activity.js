/* Durable Agent audits are independent of this process's live crawler queues. */
'use strict';
var agentActivityCharts = ['visual-agent-tokens', 'visual-agent-steps'];
function resetAgentActivity() {
    var section = document.getElementById('agent-activity');
    if (section) section.hidden = true;
    ['agent-activity-summary', 'agent-activity-note'].forEach(function (id) {
        var el = document.getElementById(id); if (el) el.textContent = '';
    });
    destroyMonitorChartGroup(agentActivityCharts);
}
function activityStatus(status) {
    return ({running:'分析中',awaiting_review:'待人工审阅',failed:'失败',interrupted:'中断',cancelled:'已取消'})[status] || status;
}
function activityTokens(run) {
    var usage = run.usage || {}, input = workbenchCount(usage.input_tokens), output = workbenchCount(usage.output_tokens);
    return input === null || output === null ? null : workbenchAdd(input, output);
}
function renderAgentActivity(data, chartsAvailable) {
    var section = document.getElementById('agent-activity'), activity = data && data.agent_activity;
    if (!section) return;
    if (!activity) { resetAgentActivity(); return; }
    section.hidden = false;
    var runs = (activity.runs || []).filter(function (run) { return currentSite === null || run.site === currentSite; });
    var note = document.getElementById('agent-activity-note');
    note.textContent = (monitorVisuals.failed ? '连接异常，保留上次快照。' : '') +
        '最近 24 小时创建的持久审计 · 每轮最多扫描 50 条 · ' + (activity.partial ? '记录未全部展示，可到运行与候选继续查询。' : '当前范围 ' + runs.length + ' 条。') +
        'Token 为模型已报告用量，不是每日额度账本；这里的验收不计入正式采集队列。' +
        (activity.error ? ' 审计读取异常，部分信息暂不可用。' : '');
    var shown = runs.slice(0, 8);
    document.getElementById('agent-activity-summary').innerHTML = shown.length ?
        '<table class="schedule-table"><thead><tr><th>站点 / 开始时间</th><th>状态</th><th>模型调用</th><th>已报告 Token</th></tr></thead><tbody>' + shown.map(function (run) {
            var calls = workbenchCount((run.usage || {}).model_calls), tokens = activityTokens(run);
            return '<tr><td title="' + esc(run.id) + '">' + esc(run.site) + '<br><small>' + esc(new Date(run.created_at).toLocaleString()) + '</small></td><td>' + esc(activityStatus(run.status)) + '</td><td>' + (calls === null ? '—' : calls) + '</td><td>' + (tokens === null ? '—' : tokens.toLocaleString()) + '</td></tr>';
        }).join('') + '</tbody></table>' + (runs.length > shown.length ? '<p class="hint">这里只展示当前页最近 8 条，更多记录请查看运行与候选。</p>' : '') :
        '<p class="hint">' + (activity.error ? '暂时无法读取运行审计。' : '当前扫描页没有匹配的 Agent 运行。') + '</p>';
    if (!chartsAvailable) { destroyMonitorChartGroup(agentActivityCharts); return; }
    showMonitorChart('visual-agent-tokens', {type:'bar',horizontal:true,title:'模型已报告用量',unit:'Token',
        description:'当前页最近 8 次分析，输入与输出合计；未知消耗不在此估算。',error:activity.error === 'audit_unavailable' ? '审计暂不可用' : null,
        data:shown.map(function (run) { return {key:run.id,label:run.site + ' · ' + new Date(run.created_at).toLocaleTimeString(),value:activityTokens(run)}; }),emptyText:'暂无分析用量'});
    var detail = activity.detail, match = detail && runs.some(function (run) { return run.id === detail.run.id; });
    var steps = match ? (detail.steps || []).slice(-12) : [];
    var stamp = Date.parse(data.generated_at);
    showMonitorChart('visual-agent-steps', {type:'bar',horizontal:true,title:'最近分析 · 步骤耗时',unit:'秒',
        description:match ? activityStatus(detail.run.status) + ' · 首批最多 100 个步骤中的最后 12 项' + (detail.next !== '0' ? '，后续步骤请查看审计' : '') +
            ' · 最终候选 ' + (detail.candidates || []).length + ' 个；' + (detail.verified ? '独立验收通过，不代表已正式发布。' : '尚未确认独立验收通过。') : '查看当前扫描页最近一条运行的持久步骤。',
        error:activity.error ? '步骤审计暂不可用' : null,
        data:steps.map(function (step) {
            var end = step.finished_at ? Date.parse(step.finished_at) : stamp, start = Date.parse(step.started_at);
            return {key:step.sequence,label:'#' + step.sequence + ' ' + (step.tool || (step.kind === 'model' ? '模型' : step.kind)) + (step.finished_at ? '' : '（进行中）'),
                value:Number.isFinite(end) && Number.isFinite(start) && end >= start ? Math.round((end-start)/10)/100 : null};
        }),emptyText:'当前范围暂无步骤；可到运行与候选查看指定运行'});
}
