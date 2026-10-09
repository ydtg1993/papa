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

(function () {
    var nativeFetch = window.fetch;

    window.fetch = function (input, init) {
        var id = sniffTraceID(input, init);
        var promise = nativeFetch.apply(this, arguments);
        if (id !== null) {
            // 动作本身的成败与抽屉无关（动作只是入口），失败也要让 oao 照常走它的错误分支
            promise.then(function () { openTrace(id); }, function () {});
        }
        return promise;
    };

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
    async function openTrace(id) {
        var resp = await traceFetch('/api/task/trace?id=' + encodeURIComponent(id));
        if (resp.error) {
            alertTrace(id, resp.error);
            return;
        }
        Dialog.open({
            title: '任务追踪 #' + id,
            body: renderSteps(resp.data.steps || []),
            actions: [{ label: '关闭', value: 'close' }]
        });
    }

    function alertTrace(id, msg) {
        Dialog.alert({
            title: '任务追踪 #' + id,
            body: '<p>' + esc(String(msg)) + '</p>'
        });
    }

    /**
     * 自己走一遍鉴权取数，而不是用 mo.js 的 apiFetch：后者把所有非 2xx 都吞成 null，
     * 而这里正需要把服务端那句「步骤追踪未开启」原样给操作人看（tokenadmin 的 tokenWrite 同理）。
     */
    async function traceFetch(url) {
        var headers = {};
        var key = loadKey();
        if (key) headers['Authorization'] = 'Bearer ' + key;
        var resp;
        try {
            resp = await fetch(url, { headers: headers });
        } catch (e) {
            return { error: '连接失败' };
        }
        if (resp.status === 401) {
            clearKey();
            setStatus(false, '需要密钥');
            showLogin(true);
            return { error: '需要密钥' };
        }
        if (!resp.ok) {
            var msg = (await resp.text()).trim();
            return { error: msg || ('HTTP ' + resp.status) };
        }
        return { data: await resp.json() };
    }

    /** 按尝试分组渲染时间线。 */
    function renderSteps(steps) {
        if (!steps.length) {
            return '<div class="trace-body"><p class="trace-empty">没有步骤记录。'
                + '可能是追踪未开启、这条任务还没被跑过，或记录已过保留期。</p></div>';
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
        return '<div class="trace-body">' + groups.map(function (g) {
            return '<section class="trace-attempt">'
                + '<h4>第 ' + (g.attempt + 1) + ' 次尝试</h4>'
                + '<ol class="trace-steps">' + g.items.map(renderStep).join('') + '</ol>'
                + '</section>';
        }).join('') + '</div>';
    }

    function renderStep(s) {
        var failed = s.status === 'failed';
        var warn = s.status === 'warn';
        var meta = fmtDuration(s.duration);
        // 失败与警告都带上错误分类（kind），成功步骤没有
        if ((failed || warn) && s.kind) meta += (meta ? ' · ' : '') + s.kind;
        var cls = failed ? ' trace-step-failed' : (warn ? ' trace-step-warn' : '');
        var html = '<li class="trace-step' + cls + '">'
            + '<div class="trace-step-head">'
            + '<span class="trace-step-name">' + esc(s.step) + '</span>'
            + (meta ? '<span class="trace-step-meta">' + esc(meta) + '</span>' : '')
            + '</div>';
        // 警告的 message 同样要显示：它就是"这一步降级在哪"的答案（任务本身是成功的）
        if ((failed || warn) && s.message) {
            html += '<pre class="trace-msg">' + esc(s.message) + '</pre>';
        }
        if (s.data) {
            html += '<details class="trace-data"><summary>采集到的数据</summary>'
                + '<pre>' + esc(prettyJSON(s.data)) + '</pre></details>';
        }
        // 引擎写的「归档页面」步骤：data.files 是这次尝试留下的现场文件 ——
        // 给每个文件一个下载按钮（拿下去本地对着真实页面写选择器，比在浏览器里看更实用）。
        var files = archivedFiles(s);
        if (files.length) {
            html += '<div class="trace-files">' + files.map(function (f) {
                return '<button class="btn" data-file="' + esc(f) + '"'
                    + ' onclick="traceDownloadPage(this.dataset.file)">下载这一页</button>';
            }).join('') + '</div>';
        }
        return html + '</li>';
    }

    /** 这一步是不是「归档页面」，是就返回它记下的文件相对路径列表。 */
    function archivedFiles(s) {
        if (!s.data) return [];
        try {
            var d = JSON.parse(s.data);
            return Array.isArray(d && d.files) ? d.files : [];
        } catch (e) {
            return [];
        }
    }

    // 走 mo.js 的 downloadFile（它带令牌、并按 Content-Disposition 定文件名）。
    // 挂在 window 上是因为本文件是 IIFE，而按钮是 innerHTML 里的内联 onclick。
    window.traceDownloadPage = async function (rel) {
        if (!rel) return;
        await downloadFile('/api/task/page?file=' + encodeURIComponent(rel));
    };

    /** Go 的 time.Duration 在 JSON 里是纳秒整数。 */
    function fmtDuration(ns) {
        if (typeof ns !== 'number' || ns <= 0) return '';
        if (ns < 1e6) return (ns / 1e3).toFixed(1) + ' ms';
        if (ns < 1e9) return Math.round(ns / 1e6) + ' ms';
        return (ns / 1e9).toFixed(2) + ' s';
    }

    function prettyJSON(text) {
        try { return JSON.stringify(JSON.parse(text), null, 2); } catch (e) { return text; }
    }
})();
