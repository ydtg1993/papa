/* shell/session: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/session",null,["Dialog","MonitorShell","Papa","document","localStorage","requestLoginRefresh","retireTableView","setMonitorVisualConnection","setQueueGovernanceConnection","setWorkbenchConnection","startPolling","stopPolling","window"],function(deps,lifecycle){
'use strict';
    /* ============ 基础状态 ============ */
    var KEY_STORAGE = 'papa_monitor_key';
    var authGeneration = 0;
    var MODULE_TITLES = { dashboard: '运行概览', queues: '队列治理', tables: '数据', pages: '自定义页', tokens: '访问令牌', obs: '运行报告', logs: '运行日志', settings: '系统设置', section: '数据与资源' };
    var currentModule = 'dashboard';

    function loadKey() {
        try { return deps.localStorage.getItem(KEY_STORAGE) || ''; }
        catch (_) { return ''; } // Unavailable storage never supplies a credential.
    }
    var credentialSeen = loadKey(), monitorViewEpoch = 0;
    function captureMonitorView() {
        var view = monitorViewEpoch, identity = authGeneration, key = loadKey(), module = currentModule;
        return function () { return view === monitorViewEpoch && identity === authGeneration && key === loadKey() && module === currentModule; };
    }
    function resetMonitorSessionState() {
        authGeneration++; monitorViewEpoch++; deps.stopPolling();
        if (typeof deps.Dialog !== 'undefined' && typeof deps.Dialog.reset === 'function') deps.Dialog.reset();
        deps.MonitorShell.reset();
        deps.retireTableView();
        if (typeof deps.Papa !== 'undefined') deps.Papa.leave(true);
    }
    function saveKey(k) { resetMonitorSessionState(); deps.localStorage.setItem(KEY_STORAGE, k); credentialSeen = k; }
    function clearKey() { resetMonitorSessionState(); deps.localStorage.removeItem(KEY_STORAGE); credentialSeen = ''; }
    function syncMonitorCredential() {
        var key = loadKey();
        if (key === credentialSeen) return false;
        resetMonitorSessionState(); credentialSeen = key; deps.requestLoginRefresh();
        if (key || !authRequired()) deps.startPolling();
        else { setStatus(false, '需要密钥'); showLogin(false); }
        return true;
    }
    lifecycle.listen(deps.window,'storage', function (event) {
        if ((event.key !== KEY_STORAGE && event.key !== null) || event.storageArea !== deps.localStorage) return;
        syncMonitorCredential();
    });
    // 服务端默认要求登录；自定义身份提供方可调整登录遮罩的展示。
    // 此标记只影响页面展示，不改变服务端对每个管理请求的认证。
    function authRequired() { return deps.document.documentElement.getAttribute('data-auth-required') === '1'; }

    function showLogin(showErr) {
        // 可见性挂在 <html> 的 need-login 上（不是遮罩自己的 .hidden）：这样 <head> 里的
        // 内联脚本能在第一帧之前就把状态定好，刷新时不会闪一下登录框
        deps.document.documentElement.classList.add('need-login');
        deps.document.getElementById('err').style.display = showErr ? 'block' : 'none';
        deps.document.getElementById('key').focus();
    }
    function hideLogin() { deps.document.documentElement.classList.remove('need-login'); }
    // 输密钥后除了拉数据，也要重建两个动态菜单 —— 否则首次访问输完密钥，
    // 侧边栏的表格页与自定义页都是空的，要手动刷新才出来
    function setStatus(ok, text) {
        deps.document.getElementById('dot').className = ok ? 'dot ok' : 'dot';
        deps.document.getElementById('statusText').textContent = text;
        if (typeof deps.setWorkbenchConnection === 'function') deps.setWorkbenchConnection(ok, text);
        if (typeof deps.setQueueGovernanceConnection === 'function') deps.setQueueGovernanceConnection(ok, text);
        if (!ok && typeof deps.setMonitorVisualConnection === 'function') deps.setMonitorVisualConnection(false);
    }


function getSessionGeneration(){return authGeneration;} function getActiveModule(){return currentModule;} function setActiveModule(name){currentModule=name;} function invalidateMonitorView(){monitorViewEpoch++;} function moduleTitle(name){return MODULE_TITLES[name];}

return Object.defineProperties({}, {
"loadKey":{enumerable:true,get:function(){return loadKey;},set:function(value){loadKey=value;}},
"captureMonitorView":{enumerable:true,get:function(){return captureMonitorView;},set:function(value){captureMonitorView=value;}},
"resetMonitorSessionState":{enumerable:true,get:function(){return resetMonitorSessionState;},set:function(value){resetMonitorSessionState=value;}},
"saveKey":{enumerable:true,get:function(){return saveKey;},set:function(value){saveKey=value;}},
"clearKey":{enumerable:true,get:function(){return clearKey;},set:function(value){clearKey=value;}},
"syncMonitorCredential":{enumerable:true,get:function(){return syncMonitorCredential;},set:function(value){syncMonitorCredential=value;}},
"authRequired":{enumerable:true,get:function(){return authRequired;},set:function(value){authRequired=value;}},
"showLogin":{enumerable:true,get:function(){return showLogin;},set:function(value){showLogin=value;}},
"hideLogin":{enumerable:true,get:function(){return hideLogin;},set:function(value){hideLogin=value;}},
"setStatus":{enumerable:true,get:function(){return setStatus;},set:function(value){setStatus=value;}},
"getSessionGeneration":{enumerable:true,get:function(){return getSessionGeneration;},set:function(value){getSessionGeneration=value;}},
"getActiveModule":{enumerable:true,get:function(){return getActiveModule;},set:function(value){getActiveModule=value;}},
"setActiveModule":{enumerable:true,get:function(){return setActiveModule;},set:function(value){setActiveModule=value;}},
"invalidateMonitorView":{enumerable:true,get:function(){return invalidateMonitorView;},set:function(value){invalidateMonitorView=value;}},
"moduleTitle":{enumerable:true,get:function(){return moduleTitle;},set:function(value){moduleTitle=value;}}
});
});
MonitorShell.install("shell/session");
