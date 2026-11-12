/* reports/controller: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("reports/controller","obs",["apiFetch","captureMonitorView","document","downloadFile","esc","fmtTime","skeletonRows"],function(deps,lifecycle){
'use strict';
var obsListSeq = 0;
    /* ============ obs 报告（obs.report.dir） ============ */
    // 后端只列报告目录、把单份产物当纯文本吐回来；这里做「选报告 → 切文件 → 看原文 / 下载」。
    // 报告里会带日志与 URL（不可信文本），一律用 textContent 放进 <pre>，不渲染 markdown。
    var OBS_FILES = ['report.md', 'report.json', 'ai-brief.md', 'ai-brief.json'];
    var obsState = { dir: '', runs: [], run: '', file: '', requestSeq: 0 };

    // obsVerdict 把 report.json 里的 verdict 翻成后台的徽标；未知/读不到都算「未判定」。
    function obsVerdict(raw) {
        if (raw === 'pass') return { label: '通过', cls: 'ok' };
        if (raw === 'warn') return { label: '带风险通过', cls: 'warn' };
        if (raw === 'fail') return { label: '不通过', cls: 'err' };
        if (raw === 'unknown') return { label: '未判定', cls: 'off' };
        return { label: '未判定', cls: 'off' };
    }

    // obsFailReasons 未通过的检查（最多 3 条）：给判定单元格补一句「为什么红」。
    function obsFailReasons(ids) {
        var top = Array.isArray(ids) ? ids.slice(0, 3) : [];
        if (top.length === 0) return '';
        return ' <span class="muted" title="未通过的检查">' + deps.esc(top.join('、')) + '</span>';
    }

    async function fetchObsReports() {
        var el = deps.document.getElementById('obs-runs');
        if (!el) return;
        var seq = ++obsListSeq, viewCurrent = deps.captureMonitorView();
        var current = function () { return seq === obsListSeq && viewCurrent(); };
        el.innerHTML = '<table><tbody>' + deps.skeletonRows(5, 3) + '</tbody></table>';
        var msg = deps.document.getElementById('obs-msg');
        if (msg) msg.textContent = '';
        var resp = await deps.apiFetch('/api/obs/reports', { current: current });
        if (!current()) return;
        if (!resp) { el.innerHTML = '<div class="empty">读取失败，请稍后重试</div>'; return; }
        var data;
        try { data = await resp.json(); } catch (_) { if (current()) el.innerHTML = '<div class="empty">报告列表格式无效，请刷新重试</div>'; return; }
        if (current()) renderObsReports(data);
    }

    function renderObsReports(data) {
        obsState.dir = (data && data.report_dir) || '';
        obsState.runs = (data && data.runs) || [];
        var dirEl = deps.document.getElementById('obs-dir');
        if (dirEl) {
            dirEl.textContent = obsState.dir
                ? ('报告目录：' + obsState.dir)
                : '报告目录未配置（obs.report.dir）：这份部署还没开报告产物。';
        }
        var el = deps.document.getElementById('obs-runs');
        if (!el) return;
        if (obsState.runs.length === 0) {
            el.innerHTML = '<div class="empty">还没有报告：跑一次 <code>papa report</code>，'
                + '或开启 obs.report.on_exit 让进程退出时自动生成。</div>';
            return;
        }
        var html = '<table><thead><tr><th>报告</th><th>判定</th><th>生成时间</th><th>文件</th><th>操作</th></tr></thead><tbody>';
        obsState.runs.forEach(function (run) {
            var v = obsVerdict(run.verdict);
            html += '<tr>'
                + '<td><b>' + deps.esc(run.name) + '</b></td>'
                + '<td><span class="badge ' + v.cls + '">' + v.label + '</span>'
                + (run.mode === 'snapshot' ? ' <span class="badge off">快照</span>' : '')
                + (run.engine_paused ? ' <span class="badge warn">引擎暂停</span>' : '')
                + obsFailReasons(run.fail_reason_top)
                + '</td>'
                + '<td>' + deps.esc(run.generated_at ? deps.fmtTime(run.generated_at) : deps.fmtTime(run.mod_time)) + '</td>'
                + '<td>' + ((run.files || []).length) + ' / ' + OBS_FILES.length + '</td>'
                + '<td><button class="btn" style="margin-top:0" data-run="' + deps.esc(run.name)
                + '" onclick="openObsReport(this.dataset.run)">查看</button></td>'
                + '</tr>';
        });
        el.innerHTML = html + '</tbody></table>';
    }

    // openObsReport 选中一份报告：默认打开 report.md（缺它时退到第一份存在的产物）。
    function openObsReport(runName) {
        var run = obsState.runs.filter(function (r) { return r.name === runName; })[0];
        if (!run) return;
        obsState.run = run.name;
        var names = (run.files || []).map(function (f) { return f.name; });
        obsState.file = names.indexOf('report.md') >= 0 ? 'report.md' : (names[0] || '');
        deps.document.getElementById('obs-viewer-sec').hidden = false;
        renderObsTabs();
        return loadObsReport();
    }

    function renderObsTabs() {
        var tabs = deps.document.getElementById('obs-tabs');
        if (!tabs) return;
        var run = obsState.runs.filter(function (r) { return r.name === obsState.run; })[0];
        var names = run ? (run.files || []).map(function (f) { return f.name; }) : [];
        tabs.innerHTML = names.map(function (name) {
            var active = name === obsState.file ? ' active' : '';
            return '<button class="obs-tab' + active + '" data-obs-file="' + deps.esc(name) + '">' + deps.esc(name) + '</button>';
        }).join('');
        tabs.querySelectorAll('[data-obs-file]').forEach(function (b) {
            b.onclick = function () { obsState.file = b.dataset.obsFile; renderObsTabs(); loadObsReport(); };
        });
        var meta = deps.document.getElementById('obs-meta');
        if (meta) meta.textContent = obsState.run ? ('报告：' + obsState.run) : '';
    }

    async function loadObsReport() {
        // 每次选择都作废旧请求，包括没有产物的报告。
        var seq = ++obsState.requestSeq;
        var viewCurrent = deps.captureMonitorView();
        var current = function () { return seq === obsState.requestSeq && viewCurrent(); };
        var pre = deps.document.getElementById('obs-content');
        if (!pre) return;
        if (!obsState.run || !obsState.file) { pre.textContent = '这份报告没有可看的产物'; return; }
        pre.textContent = '加载中…';
        var url = '/api/obs/report?run=' + encodeURIComponent(obsState.run)
            + '&file=' + encodeURIComponent(obsState.file);
        try {
            var resp = await deps.apiFetch(url, { current: current });
            if (!current()) return;
            if (!resp) { pre.textContent = '读取失败（报告可能已被清理）'; return; }
            var content = await resp.text();
            // 响应头先到不代表正文已经读完，读取正文期间也可能切换。
            if (!current()) return;
            pre.textContent = content;
        } catch (_) {
            if (current()) pre.textContent = '读取失败（报告可能已被清理）';
        }
    }

    async function downloadObsReport() {
        if (!obsState.run || !obsState.file) return;
        var current = deps.captureMonitorView(), seq = obsState.requestSeq;
        var url = '/api/obs/report?run=' + encodeURIComponent(obsState.run)
            + '&file=' + encodeURIComponent(obsState.file) + '&download=1';
        await deps.downloadFile(url, function () { return current() && seq === obsState.requestSeq; });
    }


function reset(){obsListSeq++;obsState={dir:'',runs:[],run:'',file:'',requestSeq:obsState.requestSeq+1};['obs-dir','obs-runs','obs-msg','obs-tabs','obs-meta','obs-content'].forEach(function(id){var el=deps.document.getElementById(id);if(el)el.textContent='';});var viewer=deps.document.getElementById('obs-viewer-sec');if(viewer)viewer.hidden=true;}

function unmount(){obsListSeq++;obsState.requestSeq++;}



return Object.defineProperties({}, {
"obsVerdict":{enumerable:true,get:function(){return obsVerdict;},set:function(value){obsVerdict=value;}},
"obsFailReasons":{enumerable:true,get:function(){return obsFailReasons;},set:function(value){obsFailReasons=value;}},
"fetchObsReports":{enumerable:true,get:function(){return fetchObsReports;},set:function(value){fetchObsReports=value;}},
"renderObsReports":{enumerable:true,get:function(){return renderObsReports;},set:function(value){renderObsReports=value;}},
"openObsReport":{enumerable:true,get:function(){return openObsReport;},set:function(value){openObsReport=value;}},
"renderObsTabs":{enumerable:true,get:function(){return renderObsTabs;},set:function(value){renderObsTabs=value;}},
"loadObsReport":{enumerable:true,get:function(){return loadObsReport;},set:function(value){loadObsReport=value;}},
"downloadObsReport":{enumerable:true,get:function(){return downloadObsReport;},set:function(value){downloadObsReport=value;}},
"reset":{enumerable:true,get:function(){return reset;},set:function(value){reset=value;}},
"unmount":{enumerable:true,get:function(){return unmount;},set:function(value){unmount=value;}}
});
});
MonitorShell.install("reports/controller");
