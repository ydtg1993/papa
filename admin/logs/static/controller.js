/* logs/controller: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("logs/controller","logs",["Dialog","apiFetch","document","downloadFile","fmtBytes","getActiveModule","getSessionGeneration","loadKey"],function(deps,lifecycle){
'use strict';
    /* ============ 日志 ============ */
    var logListSeq = 0, logViewSeq = 0, logDialogSeq = 0, logDialogOpen = false;
    function invalidateLogs() {
        logListSeq++; logViewSeq++; logDialogSeq++;
        if (logDialogOpen && typeof deps.Dialog !== 'undefined') deps.Dialog.close();
        logDialogOpen = false;
        ['log-list', 'log-tail-text', 'log-tail-status'].forEach(function (id) {
            var el = deps.document.getElementById(id); if (el) el.textContent = '';
        });
    }
    async function fetchLogs() {
        var el = deps.document.getElementById('log-list');
        if (!el || deps.getActiveModule() !== 'logs') return;
        var seq = ++logListSeq, epoch = deps.getSessionGeneration(), key = deps.loadKey();
        var current = function () { return seq === logListSeq && epoch === deps.getSessionGeneration() && key === deps.loadKey() && deps.getActiveModule() === 'logs'; };
        el.innerHTML = '<div class="hint">正在读取日志列表…</div>';
        var resp = await deps.apiFetch('/api/logs', { current: current });
        if (!current()) return;
        if (!resp) { el.innerHTML = '<div class="hint">读取失败，请刷新重试</div>'; return; }
        var data;
        try { data = await resp.json(); }
        catch (_) { if (current()) el.innerHTML = '<div class="hint">日志列表格式无效，请刷新重试</div>'; return; }
        if (!current()) return;
        var files = Array.isArray(data.files) ? data.files : [];
        el.innerHTML = '';
        if (files.length === 0) { el.innerHTML = '<div class="hint">日志目录为空</div>'; return; }
        files.forEach(function (f) {
            var row = deps.document.createElement('div');
            row.className = 'log-row';
            var nm = deps.document.createElement('span');
            nm.className = 'nm'; nm.textContent = f.name;
            var sz = deps.document.createElement('span');
            sz.className = 'sz'; sz.textContent = deps.fmtBytes(f.size);
            var a = deps.document.createElement('button');
            a.type = 'button'; a.className = 'log-view';
            a.textContent = '查看';
            a.onclick = function () { viewLog(f.name); };
            row.appendChild(nm); row.appendChild(sz); row.appendChild(a);
            el.appendChild(row);
        });
    }
    function viewLog(name) {
        if (deps.getActiveModule() !== 'logs') return;
        var busy = false, seq = ++logDialogSeq, view = logViewSeq, epoch = deps.getSessionGeneration(), key = deps.loadKey();
        var dialog = deps.Dialog.open({title: name + ' · 最后 200 行',
            body: '<p class="hint" id="log-tail-status" role="status">正在读取…</p><pre class="log-tail-text" id="log-tail-text"></pre>',
            actions: [{label:'刷新', onClick:function(){ refresh(); return false; }}, {label:'关闭',value:true}]});
        logDialogOpen = true;
        var status = deps.document.getElementById('log-tail-status'), content = deps.document.getElementById('log-tail-text');
        var current = function () { return logDialogOpen && seq === logDialogSeq && view === logViewSeq && epoch === deps.getSessionGeneration() && key === deps.loadKey() && deps.getActiveModule() === 'logs' && content.isConnected; };
        dialog.then(function () { if (seq === logDialogSeq) { logDialogOpen = false; logDialogSeq++; } });
        async function refresh() {
            if (busy || !current()) return;
            busy = true; status.textContent = '正在读取…';
            try {
                var response = await deps.apiFetch('/api/logs/tail?file=' + encodeURIComponent(name), { current: current });
                if (!current()) return;
                if (!response) throw new Error('读取失败，请检查登录状态');
                if (!response.ok) throw new Error((await response.text()).trim() || '读取日志失败');
                var data = await response.json();
                if (!current()) return;
                content.textContent = data.text || '暂无日志';
                status.textContent = '显示最后 ' + data.lines + ' 行' + (data.truncated ? '（达到 1 MiB 上限，开头内容已截断）' : '');
                content.scrollTop = content.scrollHeight;
            } catch (error) { if (current()) { content.textContent = ''; status.textContent = error.message; } }
            finally { busy = false; }
        }
        refresh();
    }
    async function downloadLogs() {
        var view = logViewSeq;
        await deps.downloadFile('/api/logs/download', function () { return view === logViewSeq && deps.getActiveModule() === 'logs'; });
    }


function reset(){invalidateLogs();}

function unmount(){invalidateLogs();}



return Object.defineProperties({}, {
"invalidateLogs":{enumerable:true,get:function(){return invalidateLogs;},set:function(value){invalidateLogs=value;}},
"fetchLogs":{enumerable:true,get:function(){return fetchLogs;},set:function(value){fetchLogs=value;}},
"viewLog":{enumerable:true,get:function(){return viewLog;},set:function(value){viewLog=value;}},
"downloadLogs":{enumerable:true,get:function(){return downloadLogs;},set:function(value){downloadLogs=value;}},
"reset":{enumerable:true,get:function(){return reset;},set:function(value){reset=value;}},
"unmount":{enumerable:true,get:function(){return unmount;},set:function(value){unmount=value;}}
});
});
MonitorShell.install("logs/controller");
