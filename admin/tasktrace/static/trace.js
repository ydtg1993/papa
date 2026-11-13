/* tasktrace/trace: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("tasktrace/trace","tables",["Dialog","Toast","apiFetch","downloadFile","esc","getSessionGeneration","window"],function(deps,lifecycle){
'use strict';
/**
 * Papa Monitor — 任务步骤追踪抽屉
 *
 * 任务表上的「追踪」动作只做一件事：让 oao 发一个 POST /…/task/action/trace。
 * oao 的动作成功时只能回 {"status":"ok"}、带不回数据（组件侧就是这么实现的），
 * 而 oao 也没有对外的事件钩子（只暴露 init/mount/list/render/refresh），
 * 所以这里包一层 window.fetch 嗅探那次动作请求，从请求体 {id,row,values} 里取出任务 id，
 * 再单独拉 GET /api/task/trace?id=<id> 渲染时间线。
 *
 * 必须在 mo.js 之后加载：用到它的全局件 Toast / Dialog / esc / loadKey / clearKey /
 * setStatus / showLogin。内容全部经 esc() 转义后才进 innerHTML。
 */
'use strict';

/* 只认内置「任务」表的 trace 动作，避免误伤业务表里同名的动作 */
var TRACE_ACTION_PATH = '/task/action/trace';
// Shared read-only entry for native panels; retains Trace's session/order guard.
var showTaskTrace;

(function () {
    var nativeFetch = deps.window.fetch;
    var traceSequence = 0;
    showTaskTrace = function(id,stillCurrent) { return openTrace(String(id),++traceSequence,deps.getSessionGeneration(),stillCurrent); };

    deps.window.fetch = function (input, init) {
        var currentLifecycle = lifecycle.current();
        var id = sniffTraceID(input, init);
        var sequence = id === null ? traceSequence : ++traceSequence, epoch = deps.getSessionGeneration();
        var promise = nativeFetch.apply(this, arguments);
        if (id !== null) {
            // 动作本身的成败与抽屉无关（动作只是入口），失败也要让 oao 照常走它的错误分支
            promise.then(function (response) {
                if (currentLifecycle() && response.ok && sequence === traceSequence && epoch === deps.getSessionGeneration()) openTrace(id, sequence, epoch);
            }, function () {});
        }
        return promise;
    };

    var wrappedFetch = deps.window.fetch;
    lifecycle.cleanup(function(){if(deps.window.fetch===wrappedFetch)deps.window.fetch=nativeFetch;});

    /** 识别「追踪」动作请求并取出任务 id；不是这个请求就返回 null。 */
    function sniffTraceID(input, init) {
        try {
            var url = typeof input === 'string' ? input : (input && input.url) || '';
            if (!url.endsWith(TRACE_ACTION_PATH)) return null;
            var method = (init && init.method) || (input && input.method) || 'GET';
            if (String(method).toUpperCase() !== 'POST') return null;
            var body = init && init.body;
            if (typeof body !== 'string') return null;
            var id = JSON.parse(body).id;
            return (id === null || id === undefined || id === '') ? null : String(id);
        } catch (e) {
            return null; // 嗅探失败绝不影响原本的请求
        }
    }

    /** 拉追踪数据并弹时间线。 */
    async function openTrace(id, sequence, epoch, stillCurrent) {
        var currentLifecycle = lifecycle.current();
        var currentLifecycle = lifecycle.current();
        var current = function () { return currentLifecycle() && sequence === traceSequence && epoch === deps.getSessionGeneration() && (!stillCurrent || stillCurrent()); };
        try {
            var resp = await deps.apiFetch('/api/task/trace?id=' + encodeURIComponent(id), { current: current });
            if (!resp || !current()) return;
            var data = await resp.json();
            if (!current()) return;
            deps.Dialog.open({
                title: '任务追踪 #' + id,
                body: renderSteps(data.steps || []),
                actions: [{ label: '关闭', value: 'close' }]
            });
        } catch (e) {
            if (current()) deps.Toast.err('追踪响应读取失败，请重试');
        }
    }

    /** 按尝试分组渲染时间线。 */
    function renderSteps(steps) {
        if (!steps.length) {
            return '<div class="trace-body"><p class="trace-empty">没有保留的追踪记录。'
                + '默认错误模式下正常成功不会留存；全量模式保留所有尝试。也可能尚未执行、追踪未开启或记录已过保留期。</p></div>';
        }
        var groups = [];
        var cur = null;
        steps.forEach(function (s) {
            if (!cur || cur.attempt !== s.attempt) {
                cur = { attempt: s.attempt, items: [] };
                groups.push(cur);
            }
            cur.items.push(s);
        });
        var notice = steps[0].execution_id
            ? '<p class="trace-empty">最近保留的执行：' + deps.esc(steps[0].execution_id) + '</p>'
            : '<p class="trace-empty">记录缺少执行轮次标识，按写入顺序展示。</p>';
        notice += '<p class="trace-empty">仅展示最近保留的执行记录，不代表任务当前状态或最新一次执行。</p>';
        if (steps[0].truncated) notice += '<p class="trace-empty">记录已截断，仅展示最后 500 步。</p>';
        return '<div class="trace-body">' + notice + groups.map(function (g) {
            return '<section class="trace-attempt">'
                + '<h4>第 ' + (g.attempt + 1) + ' 次尝试</h4>'
                + '<ol class="trace-steps">' + g.items.map(renderStep).join('') + '</ol>'
                + '</section>';
        }).join('') + '</div>';
    }

    function renderStep(s) {
        var failed = s.status === 'failed';
        var warn = s.status === 'warn';
        var data = stepData(s);
        var meta = fmtDuration(s.duration);
        // 失败与警告都带上错误分类（kind），成功步骤没有
        if ((failed || warn) && s.kind) meta += (meta ? ' · ' : '') + s.kind;
        // 捕获/等待的失败现场：把「超时上限」与「实际等了多久」并排放在步骤头上。
        // 这两个数字来自 data（引擎写的 timeout_ms / elapsed_ms），不是步骤的 duration ——
        // duration 是"距上一步"的时间，会把打开页面的开销也算进去。
        if (failed && data) {
            var limit = fmtMillis(data.timeout_ms);
            var waited = fmtMillis(data.elapsed_ms);
            if (limit) meta += (meta ? ' · ' : '') + '超时 ' + limit;
            if (waited) meta += (meta ? ' · ' : '') + '实等 ' + waited;
        }
        var cls = failed ? ' trace-step-failed' : (warn ? ' trace-step-warn' : '');
        var html = '<li class="trace-step' + cls + '">'
            + '<div class="trace-step-head">'
            + '<span class="trace-step-name">' + deps.esc(s.step) + '</span>'
            + (meta ? '<span class="trace-step-meta">' + deps.esc(meta) + '</span>' : '')
            + '</div>';
        // 警告的 message 同样要显示：它就是"这一步降级在哪"的答案（任务本身是成功的）
        if ((failed || warn) && s.message) {
            html += '<pre class="trace-msg">' + deps.esc(s.message) + '</pre>';
        }
        // 捕获失败的候选请求：data.candidates 是 "METHOD URL" 的样本 ——
        // 用来区分「一个请求都没发」「发了但域名换了」「路径改版」三种死法。
        if (data && Array.isArray(data.candidates) && data.candidates.length) {
            var total = typeof data.candidates_total === 'number' ? data.candidates_total : data.candidates.length;
            html += '<details class="trace-candidates">'
                + '<summary>期间发出的请求 · 共 ' + total + ' 条（样本 ' + data.candidates.length + ' 条）</summary>'
                + '<pre>' + deps.esc(data.candidates.join('\n')) + '</pre></details>';
        }
        if (s.data) {
            html += '<details class="trace-data"><summary>采集到的数据</summary>'
                + '<pre>' + deps.esc(prettyJSON(s.data)) + '</pre></details>';
        }
        // 引擎写的「归档页面」步骤：data.files 是这次尝试留下的现场文件 ——
        // 给每个文件一个下载按钮（拿下去本地对着真实页面写选择器，比在浏览器里看更实用）。
        var files = archivedFiles(s);
        if (files.length) {
            html += '<div class="trace-files">' + files.map(function (f) {
                return '<button class="btn" data-file="' + deps.esc(f) + '"'
                    + ' onclick="traceDownloadPage(this.dataset.file)">下载这一页</button>';
            }).join('') + '</div>';
        }
        return html + '</li>';
    }

    /** 解析步骤的 data（JSON 字符串）；解不出来时返回 null —— 坏值不该把整条时间线弄断。 */
    function stepData(s) {
        if (!s.data) return null;
        try {
            return JSON.parse(s.data);
        } catch (e) {
            return null;
        }
    }

    /** 这一步是不是「归档页面」，是就返回它记下的文件相对路径列表。 */
    function archivedFiles(s) {
        var d = stepData(s);
        return Array.isArray(d && d.files) ? d.files : [];
    }

    // 走 mo.js 的 downloadFile（它带令牌、并按 Content-Disposition 定文件名）。
    // 挂在 window 上是因为本文件是 IIFE，而按钮是 innerHTML 里的内联 onclick。
    deps.window.traceDownloadPage = async function (rel) {
        if (!rel) return;
        await deps.downloadFile('/api/task/page?file=' + encodeURIComponent(rel));
    };

    /** Go 的 time.Duration 在 JSON 里是纳秒整数。 */
    function fmtDuration(ns) {
        if (typeof ns !== 'number' || ns <= 0) return '';
        if (ns < 1e6) return (ns / 1e3).toFixed(1) + ' ms';
        if (ns < 1e9) return Math.round(ns / 1e6) + ' ms';
        return (ns / 1e9).toFixed(2) + ' s';
    }

    /** trace data 里的 elapsed_ms / timeout_ms 是毫秒整数（不是纳秒）。 */
    function fmtMillis(ms) {
        if (typeof ms !== 'number' || ms <= 0) return '';
        if (ms < 1000) return ms + ' ms';
        return (ms / 1000).toFixed(1).replace(/\.0$/, '') + ' s';
    }

    function prettyJSON(text) {
        try { return JSON.stringify(JSON.parse(text), null, 2); } catch (e) { return text; }
    }
})();



return Object.defineProperties({}, {
"showTaskTrace":{enumerable:true,get:function(){return showTaskTrace;},set:function(value){showTaskTrace=value;}}
});
});
MonitorShell.install("tasktrace/trace");
