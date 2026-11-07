const assert = require('node:assert/strict'), fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const source = name => fs.readFileSync(path.join(process.argv[2], name), 'utf8');
const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
const tick = () => new Promise(resolve => setImmediate(resolve));

function setup() {
    const nodes = {}, events = {}, calls = [], requests = [], lists = [], values = new Map();
    let document;
    class Element {
        constructor(tag = 'div') {
            this.tagName = tag; this.children = []; this.dataset = {}; this.attrs = {}; this.className = ''; this.style = {}; this.hidden = false; this.isConnected = true; this._html = ''; this._text = '';
            this.classList = {
                contains: key => this.className.split(/\s+/).includes(key),
                add: key => { if (!this.classList.contains(key)) this.className += ' ' + key; },
                remove: key => { this.className = this.className.split(/\s+/).filter(value => value !== key).join(' '); },
                toggle: (key, on) => { if (on === undefined) on = !this.classList.contains(key); this.classList[on ? 'add' : 'remove'](key); }
            };
        }
        get childElementCount() { return this.children.length; }
        get textContent() { return this._text + this.children.map(child => child.textContent).join(''); }
        set textContent(text) { this._text = String(text); this._html = ''; this.children = []; }
        get innerHTML() { return this._html || this._text || this.children.map(child => child.textContent).join(''); }
        set innerHTML(html) { this._html = html; this._text = ''; this.children = []; }
        appendChild(child) { child.parentNode = this; this.children.push(child); return child; }
        append(...children) { children.forEach(child => this.appendChild(child)); }
        replaceChildren(...children) { this.children = []; this._html = ''; this._text = ''; this.append(...children); }
        setAttribute(key, value) { this.attrs[key] = String(value); }
        getAttribute(key) { return this.attrs[key] || null; }
        focus() { document.activeElement = this; }
        addEventListener(key, fn) { this['on' + key] = fn; }
        contains(target) { return this === target || this.children.some(child => child.contains(target)); }
        querySelectorAll(selector) {
            const matches = el => selector.startsWith('.') && selector.slice(1).split('.').every(key => el.classList.contains(key));
            return this.children.flatMap(child => (matches(child) ? [child] : []).concat(child.querySelectorAll(selector)));
        }
        querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
    }
    const node = id => nodes[id] || (nodes[id] = new Element());
    document = { activeElement: null, body: new Element('body'), documentElement: new Element('html'), createElement: tag => new Element(tag), getElementById: id => nodes[id] || null,
        addEventListener: (key, fn) => { (events[key] || (events[key] = [])).push(fn); },
        querySelectorAll: selector => Object.values(nodes).filter(el => selector === '.module' && el.classList.contains('module')).concat(node('main-nav').querySelectorAll(selector), node('section-nav').querySelectorAll(selector)) };
    document.documentElement.setAttribute('data-auth-required', '1');
    ['main-nav', 'section-nav', 'section-title', 'section-empty', 'moduleTitle', 'site-tabs', 'site-meta', 'nav-toggle', 'key', 'err', 'theme-toggle', 'oao-view', 'page-view', 'custom-settings', 'log-list',
        'workbench-scope', 'workbench-coverage', 'workbench-issue-count', 'workbench-kpis', 'workbench-issues', 'workbench-sites', 'workbench-freshness', 'bell', 'bell-badge', 'bell-panel'].forEach(node);
    ['dashboard', 'quality', 'obs', 'logs', 'schedules', 'rules', 'agents', 'queues', 'settings', 'tokens', 'tables', 'pages', 'section'].forEach(key => { node('mod-' + key).className = 'module'; });
    ['flow', 'monitor', 'data', 'system'].forEach(key => { const button = new Element('button'); button.className = 'nav-category'; button.dataset.section = key; node('main-nav').appendChild(button); });
    node('site-tabs').appendChild(new Element('button')); node('site-meta').innerHTML = 'site detail';
    const c = vm.createContext({ console, document, window: { addEventListener() {} }, setTimeout, clearTimeout, WeakMap,
        localStorage: { getItem: key => values.get(key) || '', setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key) },
        Oao: { init() {}, destroy() {}, list: () => { const d = deferred(); lists.push(d); return d.promise; }, render: async (...args) => calls.push(['render', ...args]) },
        apiFetch: (url, opts) => { const d = deferred(); requests.push({ url, opts, ...d }); return d.promise; },
        stopPolling() {}, startPolling() {}, handleTableUnauthorized() {}, loadSchedules() {}, loadRules() {}, loadQuality() {}, loadAgents() {} });
    ['extensions.js', 'navigation.js', 'mo.js'].forEach(name => vm.runInContext(source(name), c));
    const native = { fetchSettings: c.fetchSettings, fetchTokens: c.fetchTokens, fetchObsReports: c.fetchObsReports };
    c.fetchSettings = () => calls.push(['settings']); c.fetchTokens = () => calls.push(['tokens']); c.fetchObsReports = () => calls.push(['obs']);
    let closeDialog;
    c.Dialog = { open(opts) {
        if (closeDialog) closeDialog();
        nodes['log-tail-text'] = new Element('pre'); nodes['log-tail-status'] = new Element('p');
        const d = deferred(); closeDialog = d.resolve; calls.push(['dialog', opts]); return d.promise;
    }, close() { if (closeDialog) closeDialog(); closeDialog = null; } };
    return { c, nodes, node, calls, requests, lists, values, events, Element, native };
}

(async () => {
    const s = setup(), { c, nodes } = s;
    assert.equal(c.currentSection, 'monitor'); assert.equal(nodes['section-title'].textContent, '运行监控与报告');
    assert.equal(nodes['main-nav'].children.length, 4); assert.equal(c.monitorNavigationItems().length, 10);
    assert.equal(c.monitorNavigationItems().some(item => item.key === 'task' || item.key === 'operation_log'), false, 'unregistered framework tables must not appear');
    const tables = [{ key: 'task', label: 'Tasks', group: '数据', filters: [{ name: 'site' }] }, { key: 'operation_log', label: '操作日志', group: '数据' }, { key: 'result', label: '<img src=x onerror=alert(1)>', group: '<b>Business</b>', filters: [{ name: 'site' }] }, { key: 'plain', label: 'Plain', filters: [] }];
    const pages = [{ key: 'workbench', label: '采集方案', section: 'flow', group: '业务' }, { key: 'legacy', label: 'Legacy', group: 'Old group' }, { key: 'unknown', label: 'Unknown', section: 'future' }, { key: 'prototype', label: 'Prototype', section: '__proto__' }];
    c.setMonitorNavigation(tables, pages);
    const entries = c.monitorNavigationItems();
    assert.equal(new Set(entries.map(item => item.kind + ':' + item.key)).size, entries.length, 'an entry has duplicate ownership');
    assert.equal(entries.find(item => item.key === 'task').group, '', 'framework table groups must not contradict their fixed section');
    assert.equal(entries.find(item => item.key === 'operation_log').group, '');
    const section = key => entries.find(item => item.key === key).section;
    ['schedules', 'rules', 'agents', 'queues', 'workbench'].forEach(key => assert.equal(section(key), 'flow'));
    assert.equal(section('task'), 'data');
    ['dashboard', 'quality', 'obs', 'logs'].forEach(key => assert.equal(section(key), 'monitor'));
    ['settings', 'tokens', 'operation_log'].forEach(key => assert.equal(section(key), 'system'));
    ['result', 'plain', 'legacy', 'unknown', 'prototype'].forEach(key => assert.equal(section(key), 'data'));
    c.Papa.page('workbench', () => ({})); c.openPage('workbench', '采集方案');
    assert.equal(c.currentSection, 'flow'); assert.equal(nodes['moduleTitle'].textContent, '采集方案');
    assert.equal(nodes['section-nav'].querySelectorAll('.active')[0].dataset.page, 'workbench');
    c.switchModule('quality'); assert.equal(c.currentSection, 'monitor');
    await c.openTable('operation_log', '操作日志'); assert.equal(c.currentSection, 'system');
    await c.openTable('result', 'Results'); assert.equal(c.currentSection, 'data');
    assert.equal(nodes['moduleTitle'].textContent, tables[2].label);
    assert.ok(nodes['section-nav'].children.some(el => el.textContent === tables[2].label && el.children.length === 0));
    assert.ok(nodes['section-nav'].children.some(el => el.textContent === '<b>Business</b>' && el.children.length === 0), 'group text must not become HTML');
    c.currentSite = 'site-a'; c.syncMonitorSiteScope(); assert.equal(nodes['site-tabs'].hidden, false);
    assert.equal(c.tableFilterOpts().filter.site, 'site-a');
    await c.openTable('plain', 'Plain'); assert.equal(nodes['site-tabs'].hidden, true); assert.equal(Object.keys(c.tableFilterOpts().filter).length, 0);
    c.switchModule('queues'); assert.equal(nodes['site-tabs'].hidden, true); assert.equal(c.currentSite, 'site-a');
    c.switchModule('dashboard'); assert.equal(nodes['site-tabs'].hidden, false); assert.equal(c.currentSite, 'site-a');
    let allow = false, destroys = 0;
    c.Papa.page('dirty', () => ({ beforeLeave: () => allow, destroy: () => destroys++ }));
    c.openPage('dirty', 'Unsaved');
    const title = nodes['moduleTitle'].textContent, current = c.currentSection;
    assert.equal(c.switchSection(current), true); assert.equal(c.currentPage.key, 'dirty'); assert.equal(destroys, 0, 'same category click destroyed the active draft');
    c.setNavigationOpen(true); const priorFocus = c.document.activeElement;
    assert.equal(c.switchSection('system'), false); assert.equal(c.currentSection, current); assert.equal(nodes['moduleTitle'].textContent, title);
    assert.equal(nodes['nav-toggle'].getAttribute('aria-expanded'), 'true'); assert.equal(c.document.activeElement, priorFocus);
    assert.equal(await c.openTable('task', 'Task'), false); assert.equal(c.currentModule, 'pages'); assert.equal(destroys, 0);
    allow = true; assert.equal(c.switchSection('system'), true); assert.equal(destroys, 1); assert.equal(c.currentSection, 'system');
    assert.equal(nodes['nav-toggle'].getAttribute('aria-expanded'), 'false'); assert.equal(c.document.activeElement, nodes['moduleTitle']);
    c.setMonitorNavigation([], []); c.switchSection('data'); assert.equal(c.currentModule, 'section'); assert.equal(c.currentSection, 'data');
    assert.match(nodes['section-empty'].textContent, /暂无已注册/);
    c.setMonitorNavigation([], [{ key: 'late', label: 'Late arrival' }]); assert.equal(c.currentModule, 'section'); assert.equal(nodes['section-nav'].children[0].textContent, 'Late arrival');
    c.switchSection('not-a-section'); assert.equal(c.currentModule, 'pages'); assert.equal(c.currentPage.key, 'late');
    c.setNavigationOpen(true); assert.equal(nodes['nav-toggle'].getAttribute('aria-expanded'), 'true');
    s.events.keydown.forEach(fn => fn({ key: 'Escape' })); assert.equal(nodes['nav-toggle'].getAttribute('aria-expanded'), 'false'); assert.equal(c.document.activeElement, nodes['nav-toggle']);
    c.buildWorkbench = () => ({ rows: [] }); c.workbenchHTML = new WeakMap();
    c.renderSiteTabs({ sites: {} }); c.switchModule('dashboard');
    assert.equal(nodes['site-tabs'].hidden, true, 'a module change must not resurrect vanished sites');

    // Full production renderNav accepts late metadata only for its current session/request.
    const race = setup();
    const old = race.c.renderNav(); race.c.authGeneration++; const fresh = race.c.renderNav();
    race.lists[1].resolve([{ key: 'new', label: 'New menu' }]); await tick();
    race.requests[0].resolve({ json: async () => ({ pages: [{ key: 'page', label: 'New page', section: 'flow' }] }) }); await fresh;
    race.lists[0].reject(new Error('old failure')); await old;
    assert.equal(race.c.monitorNavigation.tables[0].key, 'new'); assert.equal(race.requests.length, 1);
    const a = race.c.renderNav(), b = race.c.renderNav();
    race.lists[3].resolve([]); await tick(); race.requests[1].resolve({ json: async () => ({ pages: [] }) }); await b;
    race.lists[2].resolve([{ key: 'stale', label: 'Stale' }]); await a; assert.equal(race.c.monitorNavigation.tables.length, 0);

    // Independent log module: latest request wins, leaving/identity changes retire lists and tails.
    const logs = setup(); logs.c.switchModule('logs'); assert.equal(logs.requests[0].url, '/api/logs'); assert.equal(logs.nodes['site-tabs'].hidden, true);
    const newest = logs.c.fetchLogs(); logs.requests[1].resolve({ json: async () => ({ files: [{ name: 'latest.log', size: 1 }] }) }); await newest;
    logs.requests[0].resolve({ json: async () => ({ files: [{ name: 'old.log' }] }) }); await tick(); assert.match(logs.nodes['log-list'].textContent, /latest.log/);
    logs.c.viewLog('latest.log'); const tail = logs.nodes['log-tail-text'];
    logs.c.switchModule('dashboard'); logs.requests[2].resolve({ ok: true, json: async () => ({ text: 'late secret', lines: 1 }) }); await tick(); assert.equal(tail.textContent, '');
    logs.c.switchModule('logs'); const staleList = logs.requests[3]; logs.c.authGeneration++;
    staleList.resolve({ json: async () => ({ files: [{ name: 'old identity' }] }) }); await tick(); assert.doesNotMatch(logs.nodes['log-list'].textContent, /old identity/);
    logs.c.pendingLoginRefresh = true; logs.c.refreshAfterLogin(); assert.equal(logs.requests[4].url, '/api/logs');
    logs.requests[4].resolve({ json: async () => ({ files: [] }) }); await tick();
    logs.c.viewLog('new.log'); const dismissed = logs.nodes['log-tail-text']; logs.c.Dialog.close(); await tick();
    logs.requests[5].resolve({ ok: true, json: async () => ({ text: 'closed tail', lines: 1 }) }); await tick(); assert.equal(dismissed.textContent, '');
    const beforeSettings = logs.requests.length; logs.c.switchModule('settings'); assert.equal(logs.requests.length, beforeSettings, 'settings must not fetch logs');
    // Render real previous-identity snapshots before clearing credentials: no scope,
    // count, pause detail, expanded popup or "already seen" alarm can survive.
    const identity = setup(); vm.runInContext(source('workbench.js'), identity.c);
    const oldData = { sites: { 'old-private-site': {} }, stages: { 'old-private-site:list': { site: 'old-private-site', queue: { queue_len: 7, in_progress: 9, completed: 4, failed: 2 } } },
        queues: {}, breakers: [{ site: 'old-private-site', stage: 'list', paused: true, source: 'breaker', reason: 'old-private-reason' }] };
    function seedOldIdentity() {
        identity.c.currentSite = 'old-private-site'; identity.c.lastData = oldData;
        identity.c.renderWorkbench(oldData); identity.c.renderBell(oldData.breakers);
        identity.nodes['bell-panel'].hidden = false; identity.nodes.bell.setAttribute('aria-expanded', 'true'); identity.nodes.bell.classList.add('pulse');
        identity.nodes['workbench-freshness'].classList.add('is-stale');
        assert.match(identity.nodes['bell-panel'].innerHTML, /old-private-site/);
        assert.equal(identity.nodes['workbench-scope'].textContent, 'old-private-site');
        assert.equal(identity.nodes['workbench-issue-count'].textContent, '1');
        assert.equal(identity.c.bellSeen.length, 1);
    }
    for (const reset of [() => identity.c.saveKey('new-session'), () => identity.c.clearKey()]) {
        seedOldIdentity(); reset();
        assert.equal(identity.c.lastData, null); assert.equal(identity.c.currentSite, null);
        assert.equal(identity.nodes['workbench-scope'].textContent, '全部站点');
        assert.equal(identity.nodes['workbench-coverage'].textContent, '等待快照');
        assert.equal(identity.nodes['workbench-issue-count'].textContent, '—');
        assert.equal(identity.nodes['workbench-freshness'].classList.contains('is-stale'), false);
        for (const id of ['workbench-kpis', 'workbench-issues', 'workbench-sites', 'bell-panel']) assert.equal(identity.nodes[id].innerHTML, '');
        assert.equal(identity.nodes['bell-panel'].hidden, true); assert.equal(identity.nodes['bell-badge'].hidden, true);
        assert.equal(identity.nodes['bell-badge'].textContent, '0'); assert.equal(identity.nodes['bell-badge'].className, 'bell-badge');
        assert.equal(identity.nodes.bell.getAttribute('aria-expanded'), 'false'); assert.equal(identity.nodes.bell.classList.contains('pulse'), false);
        assert.equal(identity.c.bellSeen, null);
        identity.c.renderBell([{ site: 'new-site', paused: true, source: 'breaker' }]);
        assert.equal(identity.nodes.bell.classList.contains('pulse'), false, 'the first new-session snapshot must not compare alarms with the previous identity');
    }
    // A confirmation may already be resolved while its continuation has not run.
    // It must never submit the old target using a replacement credential/view.
    for (const action of ['remove', 'disable', 'shutdown']) {
        for (const change of ['identity', 'navigation']) {
            const old = setup(), confirmed = deferred(), writes = [];
            old.c.currentModule = action === 'shutdown' ? 'settings' : 'tokens';
            old.c.Dialog.confirm = () => confirmed.promise;
            old.c.askShutdownToken = () => confirmed.promise;
            old.c.apiPost = async (...args) => { writes.push(args); return { json: async () => ({}) }; };
            old.c.Toast = { ok() {}, info() {}, err() {} };
            old.node('shutdown-msg');
            const pending = action === 'remove' ? old.c.removeToken(7) : action === 'disable' ? old.c.setTokenEnabled(7, false) : old.c.doShutdown();
            confirmed.resolve(action === 'shutdown' ? 'old-secret' : true);
            if (change === 'identity') old.c.saveKey('replacement'); else old.c.switchModule('dashboard');
            await pending;
            assert.equal(writes.length, 0, action + ' confirmation survived ' + change + ' change');
        }
    }
    // A completed token creation under the previous identity must not disclose
    // its one-time plaintext in the new identity's dialog.
    for (const change of ['identity', 'navigation']) {
        const old = setup(), confirmed = deferred(), response = deferred(), dialogs = [];
        old.c.currentModule = 'tokens'; old.node('tk-operator').value = 'old-user'; old.node('tk-note').value = '';
        old.c.Dialog.open = opts => { dialogs.push(opts); return confirmed.promise; };
        old.c.apiPost = () => response.promise;
        const pending = old.c.openNewToken(); confirmed.resolve('ok'); await tick();
        if (change === 'identity') old.c.saveKey('replacement'); else old.c.switchModule('dashboard');
        response.resolve({ json: async () => ({ token: 'old-plaintext-secret' }) }); await pending;
        assert.equal(dialogs.length, 1, 'old token plaintext disclosed after ' + change + ' change');
    }
    // Logging in as another identity clears both visible and hidden native data.
    const privateState = setup();
    const privateNodes = ['token-table', 'wl-input', 'wl-file-info', 'wl-msg', 'shutdown-msg', 'obs-dir', 'obs-runs', 'obs-msg', 'obs-tabs', 'obs-meta', 'obs-content'];
    privateNodes.forEach(id => { privateState.node(id).innerHTML = 'old-private-value'; privateState.nodes[id].value = 'old-private-value'; });
    privateState.node('obs-viewer-sec').hidden = false;
    privateState.c.obsState.runs = [{ name: 'old-run' }]; privateState.c.obsState.run = 'old-run'; privateState.c.obsState.file = 'report.md';
    privateState.c.saveKey('replacement');
    privateNodes.forEach(id => assert.doesNotMatch(privateState.nodes[id].innerHTML, /old-private-value/, 'identity reset retained ' + id));
    assert.equal(privateState.nodes['wl-input'].value, ''); assert.equal(privateState.nodes['obs-viewer-sec'].hidden, true);
    assert.equal(privateState.c.obsState.runs.length, 0); assert.equal(privateState.c.obsState.run, '');

    // Native readers require latest request, view and identity through body completion.
    for (const kind of ['tokens', 'settings', 'obs']) {
        const race = setup(); race.c.currentModule = kind;
        ['token-table', 'wl-input', 'wl-file-info', 'obs-runs', 'obs-dir', 'obs-content', 'obs-msg'].forEach(race.node);
        const fetcher = race.native[kind === 'tokens' ? 'fetchTokens' : kind === 'settings' ? 'fetchSettings' : 'fetchObsReports'];
        const data = marker => kind === 'tokens' ? { tokens: [{ id: 1, operator: marker }] } : kind === 'settings' ? { whitelist: [marker] } : { report_dir: marker, runs: [] };
        const target = kind === 'tokens' ? 'token-table' : kind === 'settings' ? 'wl-input' : 'obs-dir';
        const read = () => kind === 'settings' ? race.nodes[target].value : race.nodes[target].innerHTML;
        const first = fetcher(), second = fetcher();
        race.requests[1].resolve({ json: async () => data('newest') }); await second;
        race.requests[0].resolve({ json: async () => data('old-value') }); await first;
        assert.match(read(), /newest/); assert.doesNotMatch(read(), /old-value/, kind + ' list accepted stale request');
        const body = deferred(), pending = fetcher(); race.requests[2].resolve({ json: () => body.promise }); await tick();
        race.c.saveKey('replacement'); body.resolve(data('old-identity')); await pending;
        assert.doesNotMatch(read(), /old-identity/, kind + ' reader accepted old identity body');
        const oldView = deferred(), detached = fetcher(); race.requests[3].resolve({ json: () => oldView.promise }); await tick();
        race.c.switchModule('dashboard'); race.c.switchModule(kind); oldView.resolve(data('old-visit')); await detached;
        assert.doesNotMatch(read(), /old-visit/, kind + ' reader accepted a response from its previous visit');
    }
    // Framework read-only data uses a server-owned marker, never just a name prefix.
    const catalog = setup(), cc = catalog.c;
    cc.setMonitorNavigation([
        { key: 'crawler_tasks', label: '任务数据', header: '<div data-papa-framework-table></div>', filters: [{ name: 'site' }] },
        { key: 'crawler_config', label: '配置数据', header: '<div data-papa-framework-table></div>' },
        { key: 'crawler_business', label: '业务自注册' }
    ], []);
    assert.equal(cc.monitorNavigationItems().find(x => x.key === 'crawler_tasks').section, 'data');
    await cc.openTable('crawler_business');
    assert.equal(catalog.requests.length, 0, 'prefix-only business table was auto-probed');
    const older = cc.openTable('crawler_tasks'), newer = cc.openTable('crawler_config');
    catalog.requests[1].resolve({ ok: true, json: async () => ({ state: 'missing', message: '尚未迁移，请执行 papa migrate' }) }); await newer;
    catalog.requests[0].resolve({ ok: true, json: async () => ({ state: 'available' }) }); await older;
    assert.match(catalog.nodes['oao-view'].textContent, /尚未迁移/);
    assert.equal(catalog.calls.filter(x => x[0] === 'render').length, 1, 'late probe rendered an old table');
    const retry = cc.openTable('crawler_config');
    catalog.requests[2].resolve({ ok: true, json: async () => ({ state: 'available' }) }); await retry;
    assert.equal(catalog.calls.filter(x => x[0] === 'render').length, 2, 'recovered table did not render');
    const pendingBody = deferred(), authRace = cc.openTable('crawler_tasks');
    catalog.requests[3].resolve({ ok: true, json: () => pendingBody.promise }); await tick();
    cc.saveKey('replacement'); pendingBody.resolve({ state: 'available' }); await authRace;
    assert.equal(catalog.calls.filter(x => x[0] === 'render').length, 2, 'old identity probe rendered data');
    const leaveRace = cc.openTable('crawler_config'); cc.switchModule('dashboard');
    catalog.requests[catalog.requests.length - 1].resolve({ ok: true, json: async () => ({ state: 'missing', message: 'old-view' }) }); await leaveRace;
    assert.notEqual(catalog.nodes['oao-view'].textContent, 'old-view');
    console.log('PASS four-section navigation, catalog probes/recovery/identity races, draft guards, site scope and log lifecycle');
})().catch(error => { console.error(error); process.exitCode = 1; });
