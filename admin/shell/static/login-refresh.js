/* shell/login-refresh: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/login-refresh",null,["Papa","document","fetchLogs","fetchObsReports","fetchSettings","fetchTokens","getActiveModule","getCurrentPage","getCurrentTable","openPage","openTable","saveKey","startPolling"],function(deps,lifecycle){
'use strict';
    var pendingLoginRefresh = false;
    function submitKey() { deps.saveKey(deps.document.getElementById('key').value); pendingLoginRefresh = true; deps.startPolling(); }
    function refreshAfterLogin() {
        if (!pendingLoginRefresh) return;
        pendingLoginRefresh = false;
        if (deps.getActiveModule() === 'tables' && deps.getCurrentTable()) deps.openTable(deps.getCurrentTable(), deps.document.getElementById('moduleTitle').textContent);
        if (deps.getActiveModule() === 'settings') { deps.Papa.leave(true); deps.Papa.renderSettings(deps.document.getElementById('custom-settings')); deps.fetchSettings(); }
        if (deps.getActiveModule() === 'pages' && deps.getCurrentPage()) deps.openPage(deps.getCurrentPage().key, deps.getCurrentPage().label);
        if (deps.getActiveModule() === 'logs') deps.fetchLogs();
        if (deps.getActiveModule() === 'tokens') deps.fetchTokens();
        if (deps.getActiveModule() === 'obs') deps.fetchObsReports();
    }

function requestLoginRefresh(){pendingLoginRefresh=true;}

return Object.defineProperties({}, {
"submitKey":{enumerable:true,get:function(){return submitKey;},set:function(value){submitKey=value;}},
"refreshAfterLogin":{enumerable:true,get:function(){return refreshAfterLogin;},set:function(value){refreshAfterLogin=value;}},
"requestLoginRefresh":{enumerable:true,get:function(){return requestLoginRefresh;},set:function(value){requestLoginRefresh=value;}}
});
});
MonitorShell.install("shell/login-refresh");
