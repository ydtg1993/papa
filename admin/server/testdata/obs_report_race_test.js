// Executes the production loader with controlled HTTP/header and body timing.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(process.argv[2], 'utf8');
const state = source.match(/var obsState = [^;]+;/)[0];
const start = source.indexOf('async function loadObsReport()');
const end = source.indexOf('async function downloadObsReport()', start);
assert.ok(start >= 0 && end > start);
const deferred = () => {
    let resolve, reject;
    const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
    return { promise, resolve, reject };
};
const response = text => ({ text: async () => text });
const flush = () => new Promise(resolve => setImmediate(resolve));

function setup() {
    const pre = { textContent: '' };
    const requests = [];
    const context = vm.createContext({
        encodeURIComponent,
        captureMonitorView: () => () => true,
        document: { getElementById: () => pre },
        apiFetch(url) { const request = deferred(); request.url = url; requests.push(request); return request.promise; },
    });
    vm.runInContext(state + '\n' + source.slice(start, end), context);
    function load(run, file = 'report.md') {
        context.obsState.run = run;
        context.obsState.file = file;
        const done = context.loadObsReport();
        done.catch(() => {});
        return done;
    }
    return { pre, requests, load };
}

(async () => {
    // Both switching reports and switching files within one report invalidate old reads.
    for (const file of ['report.md', 'report.json']) {
        const s = setup();
        const first = s.load('A');
        const latest = s.load(file === 'report.md' ? 'B' : 'A', file);
        assert.equal(s.pre.textContent, '加载中…');
        s.requests[1].resolve(response('LATEST'));
        await latest;
        s.requests[0].resolve(response('STALE'));
        await first;
        assert.equal(s.pre.textContent, 'LATEST', 'late HTTP response overwrote current report');
    }

    // Headers may arrive before switching while the old body is still streaming.
    for (const rejectBody of [false, true]) {
        const s = setup();
        const body = deferred();
        const first = s.load('A');
        s.requests[0].resolve({ text: () => body.promise });
        await flush();
        const latest = s.load('B');
        s.requests[1].resolve(response('B'));
        await latest;
        if (rejectBody) body.reject(new Error('old body failed'));
        else body.resolve('A');
        await first;
        assert.equal(s.pre.textContent, 'B', 'late body/error overwrote current report');
    }

    for (const rejectFetch of [false, true]) {
        const s = setup();
        const first = s.load('A');
        const latest = s.load('B');
        s.requests[1].resolve(response('B'));
        await latest;
        if (rejectFetch) s.requests[0].reject(new Error('old fetch failed'));
        else s.requests[0].resolve(null);
        await first;
        assert.equal(s.pre.textContent, 'B', 'obsolete failure overwrote current report');
    }

    // Returning to A must not make the first A request current again.
    {
        const s = setup();
        const first = s.load('A');
        const middle = s.load('B');
        const latest = s.load('A');
        s.requests[2].resolve(response('LATEST A'));
        await latest;
        s.requests[1].resolve(response('B'));
        s.requests[0].resolve(response('FIRST A'));
        await Promise.all([first, middle]);
        assert.equal(s.pre.textContent, 'LATEST A');
    }

    // Even selecting a report without files must invalidate its predecessor.
    {
        const s = setup();
        const first = s.load('A');
        await s.load('empty', '');
        s.requests[0].resolve(response('A'));
        await first;
        assert.equal(s.pre.textContent, '这份报告没有可看的产物');
        assert.equal(s.requests.length, 1);
    }

    // Current failures remain visible, and an older success cannot hide them.
    for (const bodyFailure of [false, true]) {
        const s = setup();
        const first = s.load('A');
        const latest = s.load('B');
        s.requests[1].resolve(bodyFailure ? { text: async () => { throw new Error('body failed'); } } : null);
        await latest;
        assert.equal(s.pre.textContent, '读取失败（报告可能已被清理）');
        s.requests[0].resolve(response('A'));
        await first;
        assert.equal(s.pre.textContent, '读取失败（报告可能已被清理）');
    }
    console.log('PASS latest report wins across HTTP/body completion, errors, file switches, A-B-A and empty reports');
})().catch(error => { console.error(error); process.exitCode = 1; });
