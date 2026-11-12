// Exercise the shipped registry and factories with separate dependency environments.
// Deliberately keep the external view/session current: invalidation must come from
// the controller lifecycle, not a helpful request mock or changed global identity.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const controllers = require('./controllers');
const context = vm.createContext({});
controllers.load(context, fs.readFileSync(path.join(process.argv[2], 'mo.js'), 'utf8'), []);
const flush = () => new Promise(resolve => setImmediate(resolve));
function deferred() {
    let resolve, reject;
    const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
    return { promise, resolve, reject };
}

// A small DOM with browser-like replacement semantics; each host has its own tree.
function element() {
    let text = '', html = '';
    const node = {
        children: [], hidden: false, isConnected: true,
        appendChild(child) { this.children.push(child); },
        querySelectorAll() { return []; },
    };
    Object.defineProperties(node, {
        textContent: { get() { return text + this.children.map(child => child.textContent).join(''); },
            set(value) { text = String(value); html = ''; this.children = []; } },
        innerHTML: { get() { return html; },
            set(value) { html = String(value); text = ''; this.children = []; } },
    });
    return node;
}
function setup(feature) {
    const nodes = new Map();
    const ids = feature === 'reports'
        ? ['obs-content', 'obs-viewer-sec', 'obs-runs', 'obs-dir', 'obs-msg', 'obs-tabs', 'obs-meta']
        : ['log-list', 'log-tail-text', 'log-tail-status'];
    ids.forEach(id => nodes.set(id, element()));
    const requests = [], downloads = [];
    const env = {
        document: { getElementById: id => nodes.get(id) || null, createElement: element },
        captureMonitorView: () => () => true,
        getSessionGeneration: () => 1, getActiveModule: () => 'logs', loadKey: () => 'same-key',
        esc: String, fmtTime: String, fmtBytes: String, skeletonRows: () => '',
        apiFetch(url, options) {
            const request = { ...deferred(), url, current: options.current };
            requests.push(request);
            // Do not suppress stale responses here: exercise the real controller guards.
            return request.promise;
        },
        downloadFile: async (url, current) => { downloads.push({ url, current: current() }); },
        Dialog: { close() {} },
    };
    const host = context.MonitorShell.create(env);
    const instance = host.install(feature + '/controller');
    const api = instance.exports;
    function start(marker) {
        if (feature === 'reports') {
            api.renderObsReports({ report_dir: marker + '-dir', runs: [{ name: marker, files: [{ name: 'report.md' }] }] });
            return api.openObsReport(marker);
        }
        return api.fetchLogs();
    }
    function response(marker, body) {
        return feature === 'reports'
            ? { text: () => body ? body.promise : Promise.resolve(marker) }
            : { json: () => body ? body.promise : Promise.resolve({ files: [{ name: marker, size: 1 }] }) };
    }
    function bodyValue(marker) { return feature === 'reports' ? marker : { files: [{ name: marker, size: 1 }] }; }
    function visible() { return nodes.get(feature === 'reports' ? 'obs-content' : 'log-list').textContent; }
    return { host, instance, api, nodes, requests, downloads, start, response, bodyValue, visible };
}

(async () => {
    for (const feature of ['reports', 'logs']) {
        // Shared factory registry, separate hosts: a request/selection in B must not
        // invalidate A, mutate its document, or replace its private report selection.
        const a = setup(feature), b = setup(feature);
        assert.notEqual(a.instance, b.instance);
        assert.notEqual(a.api, b.api);
        assert.equal(Object.hasOwn(a.api, 'obsState'), false);
        const first = a.start('HOST-A'), second = b.start('HOST-B');
        b.requests[0].resolve(b.response('HOST-B'));
        await second;
        a.requests[0].resolve(a.response('HOST-A'));
        await first;
        assert.match(a.visible(), /HOST-A/, feature + ': other instance invalidated A');
        assert.doesNotMatch(a.visible(), /HOST-B/);
        assert.match(b.visible(), /HOST-B/);
        if (feature === 'reports') {
            await a.api.downloadObsReport();
            await b.api.downloadObsReport();
            assert.match(a.downloads[0].url, /run=HOST-A&/);
            assert.match(b.downloads[0].url, /run=HOST-B&/);
            assert.equal(a.downloads[0].current, true);
        }
        a.host.reset();
        assert.equal(a.visible(), '');
        assert.match(b.visible(), /HOST-B/, feature + ': resetting A cleared B');
        const nextB = b.start('HOST-B-NEXT');
        b.requests[1].resolve(b.response('HOST-B-NEXT'));
        await nextB;
        assert.match(b.visible(), /HOST-B-NEXT/);
    }

    for (const feature of ['reports', 'logs']) {
        for (const action of ['unmount', 'reset']) {
            for (const timing of ['http', 'body', 'body-error']) {
                const s = setup(feature), body = deferred();
                const stale = s.start('OLD');
                const oldRequest = s.requests[0];
                if (timing !== 'http') {
                    let bodyStarted = false;
                    const reply = s.response('OLD', body);
                    const method = feature === 'reports' ? 'text' : 'json';
                    const read = reply[method];
                    reply[method] = () => { bodyStarted = true; return read(); };
                    oldRequest.resolve(reply);
                    await flush();
                    assert.equal(bodyStarted, true, 'test must suspend during actual response body read');
                }
                if (action === 'reset') s.host.reset();
                else s.instance.unmount();
                assert.equal(oldRequest.current(), false, feature + ': lifecycle left old request current');
                if (action === 'reset') assert.equal(s.visible(), '');
                s.instance.mount();
                const latest = s.start('CURRENT');
                s.requests[1].resolve(s.response('CURRENT'));
                await latest;
                assert.match(s.visible(), /CURRENT/);
                if (timing === 'http') oldRequest.resolve(s.response('OLD'));
                else if (timing === 'body-error') body.reject(new Error('obsolete body failed'));
                else body.resolve(s.bodyValue('OLD'));
                await stale;
                assert.match(s.visible(), /CURRENT/, feature + '/' + action + '/' + timing + ': stale completion changed view');
                assert.doesNotMatch(s.visible(), /OLD|obsolete/);
            }
        }
    }
    console.log('PASS real reports/logs factories isolate host state and reject late HTTP/body/error after unmount/reset');
})().catch(error => { console.error(error); process.exitCode = 1; });
