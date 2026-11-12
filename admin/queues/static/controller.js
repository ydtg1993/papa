/* queues/controller: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("queues/controller","queues",["Dialog","Toast","apiRequest","captureMonitorView","document","esc","fetchData","fmtDuration","getActiveModule","workbenchCount","workbenchNumber","workbenchTime"],function(deps,lifecycle){
'use strict';
    /* ---- 治理队列（错误队列 + 轮询队列，各一条全局） ---- */
    var QUEUE_KINDS = {
        error_queue: { label: '错误队列', desc: '失败任务重投', path: '/api/errorqueue/process', key: 'processed', done: '已重新投递' },
        repeat_queue: { label: '轮询队列', desc: '周期任务重投', path: '/api/repeatqueue/process', key: 'repolled', done: '已重投' }
    };
    function queueMeta(name) {
        var kind = QUEUE_KINDS[name];
        if (!kind) return null;
        return { label: kind.label, desc: kind.desc, url: kind.path, key: kind.key, done: kind.done };
    }
    // 面板上的顺序：错误队列在前、轮询队列在后（按名字字典序）
    function queueKeys(data) {
        return Object.keys(data || {}).filter(function (n) { return QUEUE_KINDS[n] !== undefined; }).sort();
    }
    function timeValid(d) { return !isNaN(d.getTime()) && d.getFullYear() >= 2000; }
    function agoSeconds(iso) {
        var d = new Date(iso);
        return (!iso || !timeValid(d)) ? null : Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
    }
    function fmtSpan(sec) {
        if (sec < 60) return sec + 's';
        if (sec < 3600) return Math.floor(sec / 60) + 'm' + (sec % 60) + 's';
        return Math.floor(sec / 3600) + 'h' + Math.floor((sec % 3600) / 60) + 'm';
    }
    function fmtAgo(iso) {
        var s = agoSeconds(iso);
        if (s === null) return '从未';
        if (s < 60) return s + ' 秒前';
        if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
        if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
        return Math.floor(s / 86400) + ' 天前';
    }
    function absTime(iso) {
        var d = new Date(iso);
        return (!iso || !timeValid(d)) ? '从未' : d.toLocaleString();
    }
    function queueStatus(q) {
        var title = q.last_error ? ' title="上次错误：' + deps.esc(q.last_error) + '"' : '';
        if (q.running) {
            var s = agoSeconds(q.started_at);
            return '<span class="badge running"' + title + '>运行中</span>'
                + '<div class="muted">已执行 ' + (s === null ? '-' : fmtSpan(s)) + '</div>';
        }
        if (typeof q.enabled !== 'boolean' || typeof q.running !== 'boolean') return '<span class="badge off">状态未知</span>';
        if (!q.enabled) return '<span class="badge off"' + title + '>自动轮询已停用</span>';
        return '<span class="badge ok"' + title + '>空闲</span>';
    }
    function renderQueues(queues) {
        var el = deps.document.getElementById('queue-gov');
        var data = queues || {};
        if (!queueKeys(data).length) { el.innerHTML = '<div class="empty">宿主尚未提供后台治理队列快照。</div>'; return; }
        var focusedQueue = el.contains(deps.document.activeElement) && deps.document.activeElement.dataset ? deps.document.activeElement.dataset.queue : null;
        el.innerHTML = '<table class="qtable"><thead><tr>'
            + '<th>队列</th><th>状态</th><th>上次执行</th><th>处理量</th><th>待处理</th><th>操作</th>'
            + '</tr></thead><tbody>'
            + queueKeys(data).map(function (name) {
                var m = queueMeta(name);
                var q = data[name] || {};
                var ran = q.runs > 0;
                var backlog = deps.workbenchTime(q.backlog_at) ? deps.workbenchCount(q.backlog) : null;
                return '<tr>'
                    + '<td><b>' + deps.esc(m.label) + '</b><div class="muted">' + deps.esc(name) + ' · ' + deps.esc(m.desc) + '</div></td>'
                    + '<td>' + queueStatus(q) + (q.last_error ? '<div class="hint queue-error">上次错误：' + deps.esc(q.last_error) + '</div>' : '') + '</td>'
                    + '<td title="完成于 ' + deps.esc(absTime(q.last_finish_at)) + '">' + deps.esc(fmtAgo(q.last_finish_at))
                    + '<div class="muted">' + (ran ? ('耗时 ' + deps.fmtDuration(q.last_duration)) : '尚未执行') + '</div></td>'
                    + '<td>' + deps.workbenchNumber(deps.workbenchCount(q.running ? q.run_processed : q.last_processed))
                    + '<div class="muted">' + (q.running ? '本轮已处理' : '上次处理') + ' · 累计 ' + deps.workbenchNumber(deps.workbenchCount(q.total_processed)) + ' · 执行 ' + deps.workbenchNumber(deps.workbenchCount(q.runs)) + ' 轮</div></td>'
                    + '<td>' + (backlog === null ? '—（未知）' : backlog > 0 ? '<span class="badge warn">' + backlog + '</span>' : '0')
                    + '<div class="muted">' + deps.esc(fmtAgo(q.backlog_at)) + '采样</div></td>'
                    // 队列名是运行期才知道的，所以走 data-* + 从 dataset 取 ——
                    // 与熔断那个「恢复」按钮同一套写法：内联属性里只有固定代码，运行期值不拼进 JS 字符串
                    + '<td><button class="btn" style="margin-top:0" data-queue="' + deps.esc(name)
                    + '" onclick="triggerQueue(this.dataset.queue)"' + (queueFlights[name] ? ' disabled' : '') + '>立即执行</button></td>'
                    + '</tr>';
            }).join('')
            + '</tbody></table>';
        if (focusedQueue) el.querySelectorAll('[data-queue]').forEach(function (b) { if (b.dataset.queue === focusedQueue && !b.disabled) b.focus(); });
    }
    var queueFlights = Object.create(null);
    async function triggerQueue(name) {
        var m = queueMeta(name);
        if (!m || queueFlights[name] || deps.getActiveModule() !== 'queues') return;
        var current = deps.captureMonitorView(), flight = {};
        queueFlights[name] = flight;
        var el = deps.document.getElementById('queue-gov-msg');
        function buttons(disabled) {
            deps.document.querySelectorAll('[data-queue]').forEach(function (button) { if (button.dataset.queue === name) button.disabled = disabled; });
        }
        buttons(true);
        try {
            var confirmed = await deps.Dialog.confirm({ title: '执行' + m.label,
                body: '<p>将扫描并重新投递符合条件的任务，作用于<b>全部站点</b>，不受当前站点筛选影响。</p><p>此操作可能启动实际采集，不是只刷新统计。</p>', okLabel: '确认全站执行' });
            if (!confirmed || !current()) return;
            el.textContent = m.label + '处理中（全部站点）...';
            var resp = await deps.apiRequest(m.url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}', current: current });
            if (!resp || !resp.ok || !current()) return;
            var data = await resp.json();
            if (!current()) return;
            deps.Toast.ok(m.label + '：' + m.done + ' ' + (data[m.key] || 0) + ' 个任务（全部站点）');
            deps.fetchData();
        } catch (e) {
            if (current()) deps.Toast.err('响应读取失败，操作结果尚未确认，请刷新队列状态');
        } finally {
            if (queueFlights[name] === flight) delete queueFlights[name];
            if (current()) { el.textContent = ''; buttons(false); }
        }
    }

function queueOperationPending(name){return !!queueFlights[name];}

return Object.defineProperties({}, {
"queueMeta":{enumerable:true,get:function(){return queueMeta;},set:function(value){queueMeta=value;}},
"queueKeys":{enumerable:true,get:function(){return queueKeys;},set:function(value){queueKeys=value;}},
"timeValid":{enumerable:true,get:function(){return timeValid;},set:function(value){timeValid=value;}},
"agoSeconds":{enumerable:true,get:function(){return agoSeconds;},set:function(value){agoSeconds=value;}},
"fmtSpan":{enumerable:true,get:function(){return fmtSpan;},set:function(value){fmtSpan=value;}},
"fmtAgo":{enumerable:true,get:function(){return fmtAgo;},set:function(value){fmtAgo=value;}},
"absTime":{enumerable:true,get:function(){return absTime;},set:function(value){absTime=value;}},
"queueStatus":{enumerable:true,get:function(){return queueStatus;},set:function(value){queueStatus=value;}},
"renderQueues":{enumerable:true,get:function(){return renderQueues;},set:function(value){renderQueues=value;}},
"triggerQueue":{enumerable:true,get:function(){return triggerQueue;},set:function(value){triggerQueue=value;}},
"queueOperationPending":{enumerable:true,get:function(){return queueOperationPending;},set:function(value){queueOperationPending=value;}}
});
});
MonitorShell.install("queues/controller");
