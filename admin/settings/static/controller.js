/* settings/controller: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("settings/controller","settings",["Dialog","Toast","apiFetch","apiPost","captureMonitorView","document"],function(deps,lifecycle){
'use strict';
    /* ============ 设置 ============ */
    var settingsRequestSeq = 0;
    async function fetchSettings() {
        var seq = ++settingsRequestSeq, viewCurrent = deps.captureMonitorView();
        var current = function () { return seq === settingsRequestSeq && viewCurrent(); };
        var resp = await deps.apiFetch('/api/settings', { current: current });
        if (!resp || !current()) return;
        var data;
        try { data = await resp.json(); } catch (_) { if (current()) deps.Toast.err('设置响应格式无效，请刷新重试'); return; }
        if (!current()) return;
        deps.document.getElementById('wl-input').value = (data.whitelist || []).join('\n');
        deps.document.getElementById('wl-file-info').textContent = data.whitelist_source
            ? ('持久化在 ' + data.whitelist_source)
            : '这个宿主没接配置存储：改动仅本次运行有效';
    }
    async function saveWhitelist() {
        var current = deps.captureMonitorView();
        var lines = deps.document.getElementById('wl-input').value.split('\n').map(function (s) { return s.trim(); }).filter(Boolean);
        var resp = await deps.apiPost('/api/settings/whitelist', { whitelist: lines });
        if (!current()) return;
        deps.document.getElementById('wl-msg').textContent = resp && resp.ok ? '已保存' : '';
        if (resp && resp.ok) deps.Toast.ok('白名单已保存');
    }
    function askShutdownToken() {
        var typed = '';
        return deps.Dialog.open({
            title: '优雅退出',
            body: '<p>确定要优雅退出爬虫进程吗？</p>'
                + '<p>引擎会等待在途任务结束，随后关闭浏览器池与数据库连接。</p>'
                + '<p class="hint">为避免误触，请填写你自己的访问令牌确认身份。</p>'
                + '<input class="input" id="shutdown-token" type="password" autocomplete="off" placeholder="访问令牌">',
            actions: [
                { label: '取消', value: false },
                {
                    label: '退出', tone: 'danger', value: true,
                    onClick: function () {
                        var el = deps.document.getElementById('shutdown-token');
                        typed = el ? el.value.trim() : '';
                        if (!typed) { deps.Toast.err('请填写访问令牌'); return false; } // false = 先别关，留着输入框
                        return true;
                    }
                }
            ]
        }).then(function (ok) { return ok ? typed : ''; });
    }

    async function doShutdown() {
        var current = deps.captureMonitorView();
        var token = await askShutdownToken();
        if (!token || !current()) return;

        deps.document.getElementById('shutdown-msg').textContent = '正在关闭...';
        var resp = await deps.apiPost('/api/settings/shutdown', { token: token });
        if (!current()) return;
        deps.document.getElementById('shutdown-msg').textContent = resp ? '已触发优雅退出' : '请求未确认，请查看提示';
        if (resp) deps.Toast.info('已触发优雅退出');
    }


function reset(){settingsRequestSeq++;['wl-file-info','wl-msg','shutdown-msg'].forEach(function(id){var el=deps.document.getElementById(id);if(el)el.textContent='';});var el=deps.document.getElementById('wl-input');if(el){el.value='';el.textContent='';}}

function unmount(){settingsRequestSeq++;}



return Object.defineProperties({}, {
"fetchSettings":{enumerable:true,get:function(){return fetchSettings;},set:function(value){fetchSettings=value;}},
"saveWhitelist":{enumerable:true,get:function(){return saveWhitelist;},set:function(value){saveWhitelist=value;}},
"askShutdownToken":{enumerable:true,get:function(){return askShutdownToken;},set:function(value){askShutdownToken=value;}},
"doShutdown":{enumerable:true,get:function(){return doShutdown;},set:function(value){doShutdown=value;}},
"reset":{enumerable:true,get:function(){return reset;},set:function(value){reset=value;}},
"unmount":{enumerable:true,get:function(){return unmount;},set:function(value){unmount=value;}}
});
});
MonitorShell.install("settings/controller");
