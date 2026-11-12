/* shell/download: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/download",null,["Toast","clearKey","document","fetch","getSessionGeneration","loadKey","showLogin"],function(deps,lifecycle){
'use strict';
    async function downloadFile(url, isCurrent) {
        var headers = {};
        var key = deps.loadKey(), epoch = deps.getSessionGeneration();
        var current = function () { return epoch === deps.getSessionGeneration() && key === deps.loadKey() && (!isCurrent || isCurrent()); };
        if (key) headers['Authorization'] = 'Bearer ' + key;
        try {
        var resp = await deps.fetch(url, { headers: headers });
        if (!current()) return;
        if (resp.status === 401) { deps.clearKey(); deps.showLogin(true); return; }
        if (!resp.ok) { deps.Toast.err('下载失败 HTTP ' + resp.status); return; }
        var blob = await resp.blob();
        if (!current()) return;
        var cd = resp.headers.get('Content-Disposition') || '';
        var name = 'logs.zip';
        var m = /filename="?([^";]+)"?/.exec(cd);
        if (m) name = m[1];
        var a = deps.document.createElement('a');
        a.href = URL.createObjectURL(blob);
        a.download = name;
        deps.document.body.appendChild(a);
        a.click();
        a.remove();
        URL.revokeObjectURL(a.href);
        } catch (e) { if (current()) deps.Toast.err('下载失败，请检查网络后重试'); }
    }



return Object.defineProperties({}, {
"downloadFile":{enumerable:true,get:function(){return downloadFile;},set:function(value){downloadFile=value;}}
});
});
MonitorShell.install("shell/download");
