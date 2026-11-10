/* Shared native API handling and monitor polling. Loaded before mo.js. */
'use strict';
var monitorPoll = { seq: 0, timer: null, controller: null, enabled: false, delay: 3000, suspended: false };

function stopPolling() {
    monitorPoll.enabled = false;
    monitorPoll.seq++;
    clearTimeout(monitorPoll.timer);
    if (monitorPoll.controller) monitorPoll.controller.abort();
}

async function responseError(resp) {
    var message = (await resp.text()).trim();
    try {
        var data = JSON.parse(message);
        message = data.message || data.error || message;
    } catch (_) { /* plain text is also a supported server response */ }
    message = String(message).slice(0, 1000) || ('HTTP ' + resp.status);
    if (resp.status === 409) message += '（请刷新当前数据后重试）';
    return message;
}

async function apiRequest(url, options) {
    options = options || {};
    var key = loadKey(), epoch = authGeneration;
    var headers = Object.assign({}, options.headers || {});
    if (key) headers.Authorization = 'Bearer ' + key;
    var current = function () { return epoch === authGeneration && key === loadKey() && (!options.current || options.current()); };
    var controller = new AbortController(), expired = false;
    var timeout;
    var deadline = new Promise(function (_, reject) {
        timeout = setTimeout(function () { expired = true; controller.abort(); reject(new Error('timeout')); }, url === '/api/browser-diagnostics' || url === '/api/browser-identities' ? 130000 : 10000);
    });
    try {
        // Buffer native API bodies within the same deadline. Returning at headers
        // would leave callers/operation locks waiting forever in json()/text().
        var pending = fetch(url, { method: options.method || 'GET', headers: headers, body: options.body, signal: controller.signal }).then(async function (response) {
            if (!current() || response.status === 401) return response;
            var body = await response.text();
            return new Response(body || null, { status: response.status, statusText: response.statusText, headers: response.headers });
        });
        var resp = await Promise.race([pending, deadline]);
        if (!current()) return null;
        if (resp.status === 401) {
            clearKey(); setStatus(false, '需要密钥'); showLogin(true); return null;
        }
        if (!resp.ok) {
            // Rule publication must distinguish a definite rejection from an
            // unknown commit outcome while retaining the same operation ID.
            if (options.rawHTTPError) return resp;
            var message = await responseError(resp);
            if (current()) Toast.err(message);
            return null;
        }
        return resp;
    } catch (e) {
        if (current()) Toast.err(options.method === 'POST' ? ((expired ? '请求超时' : '连接中断') + '，操作结果尚未确认，请刷新查看后再决定是否重试') : (expired ? '请求超时，请稍后重试' : '连接失败，请检查网络后重试'));
        return null;
    } finally {
        clearTimeout(timeout);
    }
}

// oao v1.3.4 does not pass the failed request's credential to this hook.
// Validate the current session before logging out; never replay a table write.
var tableAuthProbe = null;
function handleTableUnauthorized() {
    var epoch = authGeneration, key = loadKey();
    if (tableAuthProbe && tableAuthProbe.epoch === epoch && tableAuthProbe.key === key) return tableAuthProbe.promise;
    var probe = { epoch: epoch, key: key };
    probe.promise = apiFetch('/api/monitor').then(function () { return false; }).finally(function () {
        if (tableAuthProbe === probe) tableAuthProbe = null;
    });
    tableAuthProbe = probe;
    return probe.promise;
}
function apiFetch(url, options) { return apiRequest(url, options); }
function apiPost(url, body) {
    return apiRequest(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });
}

async function fetchData() {
    clearTimeout(monitorPoll.timer);
    if (document.hidden || monitorPoll.suspended) return;
    var seq = ++monitorPoll.seq, epoch = authGeneration, key = loadKey();
    if (monitorPoll.controller) monitorPoll.controller.abort();
    var controller = new AbortController();
    monitorPoll.controller = controller;
    var expired = false;
    var timeout = setTimeout(function () { expired = true; controller.abort(); }, 10000);
    var current = function () { return seq === monitorPoll.seq && epoch === authGeneration && key === loadKey(); };
    var success = false;
    try {
        var headers = key ? { Authorization: 'Bearer ' + key } : {};
        var resp = await fetch('/api/monitor', { headers: headers, signal: controller.signal });
        if (!current()) return;
        if (resp.status === 401) {
            clearKey(); setStatus(false, '需要密钥'); showLogin(true); return;
        }
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        var data = await resp.json();
        if (!current()) return;
        if (expired) throw new Error('请求超时');
        hideLogin(); setStatus(true, '已连接');
        document.getElementById('refreshTime').textContent = '刷新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
        if (typeof acceptMonitorObservation === 'function') acceptMonitorObservation(data);
        renderAll(data);
        if (typeof refreshAfterLogin === 'function') refreshAfterLogin();
        success = true;
    } catch (e) {
        if (current()) setStatus(false, expired ? '请求超时，稍后重试' : (e.message || '连接失败'));
    } finally {
        clearTimeout(timeout);
        if (current()) {
            monitorPoll.controller = null;
            monitorPoll.delay = success ? 3000 : Math.min(monitorPoll.delay * 2, 30000);
            if (monitorPoll.enabled && !document.hidden) monitorPoll.timer = setTimeout(fetchData, monitorPoll.delay);
        }
    }
}
function startPolling() {
    monitorPoll.enabled = true;
    monitorPoll.delay = 3000;
    if (monitorPoll.suspended) return;
    fetchData();
    renderNav();
}
document.addEventListener('visibilitychange', function () {
    if (document.hidden) {
        monitorPoll.seq++;
        clearTimeout(monitorPoll.timer);
        if (monitorPoll.controller) monitorPoll.controller.abort();
    } else if (monitorPoll.enabled) fetchData();
});
if (typeof window !== 'undefined') {
    window.addEventListener('pagehide', function () {
        monitorPoll.suspended = true;
        monitorPoll.seq++;
        clearTimeout(monitorPoll.timer);
        if (monitorPoll.controller) monitorPoll.controller.abort();
    });
    window.addEventListener('pageshow', function () {
        if (!monitorPoll.suspended) return;
        monitorPoll.suspended = false;
        // A frozen BFCache document may not receive another tab's storage event.
        // Clear the old identity before any cached chart can become visible.
        var changed = typeof syncMonitorCredential === 'function' && syncMonitorCredential();
        if (typeof resumeMonitorVisuals === 'function') resumeMonitorVisuals();
        if (!changed && monitorPoll.enabled) startPolling();
    });
}
