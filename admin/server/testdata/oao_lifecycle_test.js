const controllers=require('./controllers');
// Run the actual linked oao asset, not a mock renderer or a copied action body.
const assert = require('node:assert/strict'), fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const makeDOM = require('./oao_dom');
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
    assert.equal(typeof modern.c.Oao.destroy, 'function', 'linked oao must support lifecycle disposal');
        
        assert.equal(modern.c.fetch, modern.nativeFetch, 'modern oao transport must not be wrapped');
        await modern.c.Oao.render(modern.root, 'task', {filter: {site: 'modern-site'}});
        assert.ok(modern.root.querySelector('[data-action]'), 'modern action metadata was stripped');
        assert.ok(modern.requests.some(request => request.url.searchParams.get('filter[site]') === 'modern-site'));
        modern.root.querySelector('[data-action]').click(); await tick();
        assert.equal(modern.requests.filter(request => request.init.method === 'POST').length, 1, 'modern table action did not submit');

        const confirmation = setup({key: 'retry', label: 'Retry', confirm: 'Confirm?'});
        await confirmation.c.Oao.render(confirmation.root, 'task');
        const oldButton = confirmation.root.querySelector('[data-action]'); oldButton.click(); await tick();
        assert.ok(confirmation.doc.querySelector('.oao-mask.open'));
        confirmation.rotate(); confirmation.c.retireTableView(); await tick();
        assert.equal(confirmation.doc.querySelector('.oao-mask.open'), null);
        oldButton.click(); await tick();
        assert.equal(confirmation.requests.filter(request => request.init.method === 'POST').length, 0, 'retired modern action submitted under replacement identity');
        assert.equal(confirmation.root.innerHTML, '');

        for (const stale of ['401', 'body']) {
            const session = setup(), pending = deferred(); 
            session.control.next = stale === '401' ? () => pending.promise : () => Promise.resolve({ok: true, status: 200, json: () => pending.promise});
            const old = session.c.Oao.render(session.root, 'task'); await tick();
            session.rotate(); session.c.retireTableView(); await session.c.Oao.render(session.root, 'task');
            pending.resolve(stale === '401' ? new Response('unauthorized', {status: 401}) : {tables: [{key: 'task', label: 'old identity secret', columns: [], filters: [], actions: []}]});
            await old; await tick();
            assert.equal(session.unauthorized(), 0, 'retired modern response invoked unauthorized hook');
            assert.match(session.root.innerHTML, />7</); assert.doesNotMatch(session.root.innerHTML, /old identity secret/);
        }
        console.log('PASS actual modern oao: writable transport, site filtering, retired confirmations and stale-identity isolation');

})().catch(error => { console.error(error); process.exitCode = 1; });
