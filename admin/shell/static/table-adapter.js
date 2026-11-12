/* shell/table-adapter: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/table-adapter",null,["Oao","document","getSessionGeneration","loadKey","window"],function(deps,lifecycle){
'use strict';
var tableViewEpoch=0;
    var legacyTableAdapter = false;
    var LEGACY_TABLE_NOTICE = '当前表格组件版本较旧，仅支持查询；升级 oao 后可使用表格操作';
    function installLegacyTableAdapter() {
        if (legacyTableAdapter || typeof deps.Oao.destroy === 'function') return;
        legacyTableAdapter = true;
        // Old oao has no action cancellation API. Give it a private, read-only
        // transport instead of relying on hidden buttons or detached DOM nodes.
        var prefix = '/api/papa-oao-readonly', nativeFetch = deps.window.fetch;
        var origin = new URL(deps.window.location.href).origin;
        deps.window.fetch = function (input, init) {
            var raw = typeof input === 'string' ? input : input && input.url;
            var url;
            try { url = new URL(raw, deps.window.location.href); } catch (_) { return nativeFetch.apply(this, arguments); }
            if (url.origin !== origin || url.pathname.indexOf(prefix + '/') !== 0) return nativeFetch.apply(this, arguments);
            var suffix = url.pathname.slice(prefix.length), method = String(init && init.method || input && input.method || 'GET').toUpperCase();
            // Only table metadata and a single table's list route are allowed.
            // In particular no action, encoded separator, or subresource reaches the server.
            if (method !== 'GET' || !/^\/[a-zA-Z0-9_-]+$/.test(suffix)) {
                return Promise.resolve(new Response(LEGACY_TABLE_NOTICE, { status: 403, headers: { 'Content-Type': 'text/plain; charset=utf-8' } }));
            }
            var receiver = this;
            return (async function () {
                var epoch = deps.getSessionGeneration(), key = deps.loadKey();
                var current = function () { return epoch === deps.getSessionGeneration() && key === deps.loadKey(); };
                url.pathname = '/api/oao' + suffix;
                var request = typeof Request !== 'undefined' && input instanceof Request ? new Request(url.href, input) : url.href;
                var resp = await nativeFetch.call(receiver, request, init);
                // A stale 401 must not invoke the old library's unauthorized hook.
                if (!current()) return new Response('会话已变化，请重新查询', { status: 409 });
                if (!resp.ok) return resp;
                var data = await resp.json();
                if (!current()) return new Response('会话已变化，请重新查询', { status: 409 });
                if (data && Array.isArray(data.tables)) data.tables.forEach(function (table) { if (table) table.actions = []; });
                if (data && typeof data === 'object') data.actions = [];
                var headers = new Headers(resp.headers);
                headers.delete('Content-Length'); headers.delete('Content-Encoding');
                return new Response(JSON.stringify(data), { status: resp.status, statusText: resp.statusText, headers: headers });
            })();
        };
        var readonlyFetch = deps.window.fetch;
        lifecycle.cleanup(function(){if(deps.window.fetch===readonlyFetch)deps.window.fetch=nativeFetch;});
        deps.Oao.init({ base: prefix });
        var view = deps.document.getElementById('oao-view');
        if (view && view.parentNode) {
            var notice = deps.document.createElement('p');
            notice.id = 'legacy-table-notice'; notice.className = 'hint'; notice.setAttribute('role', 'status');
            notice.textContent = LEGACY_TABLE_NOTICE;
            view.parentNode.insertBefore(notice, view);
        }
    }
    function retireTableView() {
        tableViewEpoch++;
        var el = deps.document.getElementById('oao-view');
        if (!el) return;
        if (typeof deps.Oao !== 'undefined' && typeof deps.Oao.destroy === 'function') deps.Oao.destroy(el);
        else {
            // Escape uses the legacy component's own cancellation handlers:
            // pending confirmations settle and body-mounted selects close.
            if (deps.document.querySelector && deps.document.querySelector('.oao-mask.open, .oao-select-content:not([hidden])')) {
                deps.document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
            }
            // Isolate late read responses; all legacy writes are blocked above.
            if (el.parentNode && el.cloneNode) el.parentNode.replaceChild(el.cloneNode(false), el);
            else el.innerHTML = '';
        }
    }

function nextTableView(){return ++tableViewEpoch;} function getTableViewRevision(){return tableViewEpoch;}

return Object.defineProperties({}, {
"installLegacyTableAdapter":{enumerable:true,get:function(){return installLegacyTableAdapter;},set:function(value){installLegacyTableAdapter=value;}},
"retireTableView":{enumerable:true,get:function(){return retireTableView;},set:function(value){retireTableView=value;}},
"nextTableView":{enumerable:true,get:function(){return nextTableView;},set:function(value){nextTableView=value;}},
"getTableViewRevision":{enumerable:true,get:function(){return getTableViewRevision;},set:function(value){getTableViewRevision=value;}}
});
});
MonitorShell.install("shell/table-adapter");
