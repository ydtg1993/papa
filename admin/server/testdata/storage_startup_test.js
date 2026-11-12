const controllers=require('./controllers');
'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict'), fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const makeDOM = require('./legacy_oao_dom');
const source = name => fs.readFileSync(path.join(process.argv[2], name), 'utf8');

function setup(failure) {
    const {doc, create} = makeDOM(), events = {}, requests = [], timers = new Map();
    let blocked = true, nextTimer = 0, tableOptions;
    doc.documentElement = create('html');
    doc.documentElement.setAttribute('data-auth-required', '1');
    doc.documentElement.setAttribute('data-theme', 'light');
    doc.hidden = false;
    for (const id of ['key', 'err', 'theme-toggle']) {
        const el = create(id === 'theme-toggle' ? 'button' : 'input');
        el.id = id; doc.body.appendChild(el);
    }
    const storage = {
        getItem(key) {
            if (blocked && failure === 'read') throw Error('storage read blocked');
            return key === 'papa_monitor_key' ? 'synthetic-credential' : null;
        },
        setItem() { throw Error('test must not persist credentials'); }
    };
    const sandbox = {
        console, document: doc, AbortController,
        setTimeout(fn) { const id = ++nextTimer; timers.set(id, fn); return id; },
        clearTimeout(id) { timers.delete(id); },
        fetch(url, options) { requests.push({url, options}); throw Error('unauthenticated startup must not fetch'); },
        Oao: {init(options) { tableOptions = options; }, destroy() {}}
    };
    sandbox.window = sandbox;
    sandbox.addEventListener = (name, fn) => { (events[name] ||= []).push(fn); };
    Object.defineProperty(sandbox, 'localStorage', {get() {
        if (blocked && failure === 'getter') throw Error('storage access blocked');
        return storage;
    }});
    const context = vm.createContext(sandbox);
    // Execute the entire production startup, including credentialSeen, Oao.init
    // and the final login/polling branch; do not slice off the theme prefix.
    for (const file of ['requests.js', 'extensions.js', 'appearance.js', 'mo.js']) {
        controllers.run(source(file), context, {filename: file});
    }
    return {context, doc, requests, timers, tableOptions, block(value) { blocked = value; }};
}

for (const failure of ['getter', 'read']) {
    test('full monitor startup remains logged out when storage ' + failure + ' throws', () => {
        const h = setup(failure), c = h.context;
        assert.equal(c.loadKey(), '');
        assert.equal(c.syncMonitorCredential(), false, 'blocked storage must not invent a credential change');
        assert.equal(c.authRequired(), true);
        assert.equal(h.doc.documentElement.classList.contains('need-login'), true);
        assert.equal(h.doc.activeElement, h.doc.getElementById('key'));
        assert.equal(c.pollingStatus().enabled, false);
        assert.equal(h.requests.length, 0);
        assert.equal(h.timers.size, 0);
        assert.deepEqual(Object.keys(h.tableOptions.headers()), []);
        h.doc.getElementById('theme-toggle').click();
        assert.equal(c.getTheme(), 'dark');
        h.doc.getElementById('theme-toggle').click();
        assert.equal(c.getTheme(), 'light');
        c.MonitorAppearance.set('classic');
        assert.equal(c.MonitorAppearance.get(), 'classic');
        assert.equal(h.doc.documentElement.classList.contains('need-login'), true);
        assert.equal(h.requests.length, 0);
    });

    test('credential reads never reuse a prior readable value after storage ' + failure + ' becomes blocked', () => {
        const h = setup(failure);
        h.block(false);
        assert.equal(h.context.loadKey(), 'synthetic-credential');
        assert.equal(h.tableOptions.headers().Authorization, 'Bearer synthetic-credential');
        h.block(true);
        assert.equal(h.context.loadKey(), '');
        assert.deepEqual(Object.keys(h.tableOptions.headers()), []);
        assert.equal(h.context.pollingStatus().enabled, false);
        assert.equal(h.requests.length, 0);
    });
}
