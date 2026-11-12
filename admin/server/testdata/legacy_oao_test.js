const controllers=require('./controllers');
// Run the actual linked oao asset, not a mock renderer or a copied action body.
const assert = require('node:assert/strict'), fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const makeDOM = require('./legacy_oao_dom');
const mo = fs.readFileSync(path.join(process.argv[2], 'mo.js'), 'utf8');
const oao = fs.readFileSync(path.join(process.argv[2], 'oao.js'), 'utf8');
const tick = () => new Promise(resolve => setImmediate(resolve));
const deferred = () => { let resolve; const promise = new Promise(done => resolve = done); return {promise, resolve}; };

function setup(action = {key: 'retry', label: 'Retry'}) {
    const dom = makeDOM(), requests = [], control = {}, columns = [{name: 'id', label: 'ID'}];
    const filters = [{name: 'site', label: 'Site', widget: 'text', op: 'eq'}];
    let credential = 'old', unauthorized = 0;
    const payload = url => url.pathname.endsWith('/tables')
        ? {tables: [{key: 'task', label: 'Tasks', columns, filters, actions: [action]}]}
        : {columns, filters, actions: [action], rows: [{id: 7}], total: 1, page: 1, size: 20};
    const nativeFetch = function(input, init) {
        const url = new URL(typeof input === 'string' ? input : input.url, 'http://monitor.test/');
        const call = {input, init, url, receiver: this}; requests.push(call);
        if (control.next) { const handler = control.next; control.next = null; return handler(call); }
        return Promise.resolve(new Response(JSON.stringify(payload(url)), {status: 200, headers: {'Content-Type': 'application/json'}}));
    };
    const c = vm.createContext({console, document: dom.doc, URL, Request, Response, Headers, AbortController,
        fetch: nativeFetch, location: {href: 'http://monitor.test/monitor'}, addEventListener() {}, setTimeout, clearTimeout,
        CustomEvent: function(type, init) { this.type = type; Object.assign(this, init); },
        KeyboardEvent: function(type, init) { this.type = type; Object.assign(this, init); },
        loadKey: () => credential, authGeneration: 0, tableViewEpoch: 0});
    c.window = c; controllers.run(oao, c); controllers.load(c,mo,['shell/table-adapter']);
    c.Oao.init({base: '/api/oao', headers: () => ({Authorization: 'Bearer ' + credential}), onUnauthorized: () => { unauthorized++; return false; }});
    const root = dom.doc.body.appendChild(dom.create('div')); root.id = 'oao-view';
    return {c, ...dom, root, requests, control, nativeFetch, payload, unauthorized: () => unauthorized,
        rotate() { credential = 'new'; c.authGeneration++; }};
}

(async () => {
    const modern = setup();
    if (typeof modern.c.Oao.destroy === 'function') {
        modern.c.installLegacyTableAdapter();
        assert.equal(modern.c.fetch, modern.nativeFetch, 'modern oao transport must not be wrapped');
        assert.equal(modern.doc.getElementById('legacy-table-notice'), null);
        await modern.c.Oao.render(modern.root, 'task', {filter: {site: 'modern-site'}});
        assert.ok(modern.root.querySelector('[data-action]'), 'modern action metadata was stripped');
        assert.ok(modern.requests.some(request => request.url.searchParams.get('filter[site]') === 'modern-site'));
        modern.root.querySelector('[data-action]').click(); await tick();
        assert.equal(modern.requests.filter(request => request.init.method === 'POST').length, 1, 'modern table action did not submit');

        const confirmation = setup({key: 'retry', label: 'Retry', confirm: 'Confirm?'});
        confirmation.c.installLegacyTableAdapter(); await confirmation.c.Oao.render(confirmation.root, 'task');
        const oldButton = confirmation.root.querySelector('[data-action]'); oldButton.click(); await tick();
        assert.ok(confirmation.doc.querySelector('.oao-mask.open'));
        confirmation.rotate(); confirmation.c.retireTableView(); await tick();
        assert.equal(confirmation.doc.querySelector('.oao-mask.open'), null);
        oldButton.click(); await tick();
        assert.equal(confirmation.requests.filter(request => request.init.method === 'POST').length, 0, 'retired modern action submitted under replacement identity');
        assert.equal(confirmation.root.innerHTML, '');

        for (const stale of ['401', 'body']) {
            const session = setup(), pending = deferred(); session.c.installLegacyTableAdapter();
            session.control.next = stale === '401' ? () => pending.promise : () => Promise.resolve({ok: true, status: 200, json: () => pending.promise});
            const old = session.c.Oao.render(session.root, 'task'); await tick();
            session.rotate(); session.c.retireTableView(); await session.c.Oao.render(session.root, 'task');
            pending.resolve(stale === '401' ? new Response('unauthorized', {status: 401}) : {tables: [{key: 'task', label: 'old identity secret', columns: [], filters: [], actions: []}]});
            await old; await tick();
            assert.equal(session.unauthorized(), 0, 'retired modern response invoked unauthorized hook');
            assert.match(session.root.innerHTML, />7</); assert.doesNotMatch(session.root.innerHTML, /old identity secret/);
        }
        console.log('PASS actual modern oao: writable transport, site filtering, retired confirmations and stale-identity isolation');
        return;
    }

    const read = setup(); read.c.installLegacyTableAdapter();
    assert.equal(read.doc.getElementById('legacy-table-notice').textContent, '当前表格组件版本较旧，仅支持查询；升级 oao 后可使用表格操作');
    const tables = await read.c.Oao.list(true);
    assert.equal(tables[0].actions.length, 0);
    assert.equal(tables[0].filters[0].name, 'site');
    await read.c.Oao.render(read.root, 'task', {filter: {site: 'site-a'}});
    assert.equal(read.root.querySelector('[data-action]'), null, 'server actions must be removed before old oao receives metadata');
    assert.match(read.root.innerHTML, />7</);
    assert.ok(read.requests.some(request => request.url.searchParams.get('filter[site]') === 'site-a'));
    read.root.querySelector('[data-go]').click(); await tick();
    assert.ok(read.requests.at(-1).url.searchParams.has('page'), 'query pagination remains active');
    read.root.querySelector('[data-pagesize]').click();
    assert.equal(read.doc.querySelector('.oao-select-content').hidden, false);
    read.c.retireTableView();
    assert.equal(read.doc.querySelector('.oao-select-content').hidden, true, 'legacy body-mounted popup survived retirement');

    // The adapter owns only its exact same-origin virtual prefix. Other callers
    // receive the original Promise and unchanged arguments/receiver.
    for (const input of ['/api/monitor', '/api/papa-oao-readonly-extra/task', '/api/oao/task/action/retry', 'https://other.test/api/papa-oao-readonly/task']) {
        const pending = deferred(), init = {method: 'POST', body: 'unchanged'}, receiver = {};
        read.control.next = () => pending.promise;
        const actual = read.c.fetch.call(receiver, input, init);
        assert.equal(actual, pending.promise); assert.equal(read.requests.at(-1).input, input);
        assert.equal(read.requests.at(-1).init, init); assert.equal(read.requests.at(-1).receiver, receiver);
        pending.resolve(new Response('ok')); await actual;
    }
    const beforeBlocked = read.requests.length;
    for (const [url, method] of [
        ['/api/papa-oao-readonly/task/action/retry', 'POST'], ['/api/papa-oao-readonly/task', 'POST'],
        ['/api/papa-oao-readonly/tables', 'DELETE'], ['/api/papa-oao-readonly/task/action/retry', 'GET'],
        ['/api/papa-oao-readonly/task%2Faction%2Fretry', 'GET']
    ]) assert.equal((await read.c.fetch(url, {method})).status, 403);
    assert.equal(read.requests.length, beforeBlocked, 'blocked writes/subresources must never reach native fetch');
    const requestInput = new Request('http://monitor.test/api/papa-oao-readonly/task?filter[site]=request-site', {headers: {Authorization: 'Bearer old'}});
    const requestResponse = await read.c.fetch(requestInput);
    assert.equal(requestResponse.status, 200);
    assert.ok(read.requests.at(-1).input instanceof Request);
    assert.equal(read.requests.at(-1).url.pathname, '/api/oao/task');
    assert.equal(read.requests.at(-1).url.searchParams.get('filter[site]'), 'request-site');

    // Keep a real pre-adapter confirmation pending, then enter the safe transport
    // and retire it. Reopening the same action proves its in-flight Promise was
    // cancelled and its private action lock released, not merely hidden in DOM.
    const confirmation = setup({key: 'retry', label: 'Retry', confirm: 'Confirm?'});
    await confirmation.c.Oao.render(confirmation.root, 'task');
    const oldButton = confirmation.root.querySelector('[data-action]'); oldButton.click(); await tick();
    assert.ok(confirmation.doc.querySelector('.oao-mask.open'));
    confirmation.c.installLegacyTableAdapter(); confirmation.rotate(); confirmation.c.retireTableView(); await tick();
    assert.equal(confirmation.doc.querySelector('.oao-mask.open'), null);
    oldButton.click(); await tick();
    assert.ok(confirmation.doc.querySelector('.oao-mask.open'), 'confirmation Promise did not release its action lock');
    confirmation.c.retireTableView(); await tick();
    assert.equal(confirmation.requests.filter(request => request.init.method === 'POST').length, 0);

    // Force an action retained from old metadata to bypass the missing buttons.
    // Even while its Promise is pending across retirement/re-entry, no POST can
    // enter the native transport and no old success can refresh the new table.
    const forced = setup(); await forced.c.Oao.render(forced.root, 'task');
    const retainedAction = forced.root.querySelector('[data-action]'); forced.c.installLegacyTableAdapter();
    retainedAction.click(); forced.rotate(); forced.c.retireTableView();
    const replacement = forced.doc.getElementById('oao-view');
    await forced.c.Oao.render(replacement, 'task', {filter: {site: 'new-site'}});
    const afterRender = forced.requests.length; await tick();
    assert.equal(forced.requests.filter(request => request.init.method === 'POST').length, 0);
    assert.equal(forced.requests.length, afterRender, 'blocked old action refreshed the replacement view');
    assert.equal(forced.doc.body.querySelectorAll('.oao-toast').some(toast => toast.textContent.includes('成功')), false);

    for (const stale of ['401', 'body']) {
        const session = setup(); session.c.installLegacyTableAdapter(); const pending = deferred();
        session.control.next = stale === '401' ? () => pending.promise : () => Promise.resolve({ok: true, status: 200, headers: new Headers(), json: () => pending.promise});
        const old = session.c.Oao.render(session.root, 'task'); await tick();
        session.rotate(); session.c.retireTableView();
        const next = session.doc.getElementById('oao-view'); await session.c.Oao.render(next, 'task');
        pending.resolve(stale === '401' ? new Response('unauthorized', {status: 401}) : {tables: [{key: 'task', label: 'old identity secret', columns: [], filters: [], actions: []}]});
        await old; await tick();
        assert.equal(session.unauthorized(), 0, 'stale 401 invoked the old library auth hook');
        assert.doesNotMatch(next.innerHTML, /old identity secret|会话已变化/);
        assert.match(next.innerHTML, />7</);
    }
    console.log('PASS actual legacy oao: query compatibility, cancelled confirmations, network-blocked writes and stale-identity isolation');
})().catch(error => { console.error(error); process.exitCode = 1; });
