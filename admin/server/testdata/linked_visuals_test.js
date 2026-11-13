const controllers=require('./controllers');
// Integration contract: Papa runs against the actual linked oao assets, with
// eventful DOM/animation/observer doubles and no copied chart implementation.
'use strict';
const assert = require('node:assert/strict'), fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const makeDOM = require('./oao_dom');
const staticDir = process.argv[2], oaoDir = process.argv[3] || staticDir;
const scripts = ['oao-motion.js', 'oao-charts.js'];
if (scripts.some(name => !fs.existsSync(path.join(oaoDir, name)))) {
    console.log('SKIP linked oao does not provide optional charts and motion');
    process.exit(0);
}

function setup() {
    const dom = makeDOM(), doc = dom.doc, proto = Object.getPrototypeOf(dom.create('div'));
    const frames = new Map(), observers = new Set(), animations = new Set(), events = {};
    let frameID = 0;
    Object.defineProperty(proto, 'firstChild', {get() { return this.children[0] || null; }});
    Object.defineProperty(proto, 'ownerDocument', {get() { return doc; }});
    const originalSetAttribute = proto.setAttribute;
    proto.setAttribute = function(key, value) {
        originalSetAttribute.call(this, key, value);
        if (key.startsWith('data-')) this.dataset[key.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())] = String(value);
    };
    proto.closest = function(selector) {
        for (let at = this; at && at.tagName; at = at.parentNode) {
            if (at.parentNode && at.parentNode.querySelectorAll && at.parentNode.querySelectorAll(selector).includes(at)) return at;
        }
        return null;
    };
    proto.focus = function(options) {
        if (this.isConnected) { doc.activeElement = this; this.focusOptions = options; }
    };
    function removeEventListener(type, listener) { this.listeners[type] = (this.listeners[type] || []).filter(fn => fn !== listener); }
    proto.removeEventListener = removeEventListener; doc.removeEventListener = removeEventListener;
    proto.animate = function() {
        const animation = {el: this, cancel() { animations.delete(this); if (this.oncancel) this.oncancel(); }};
        animations.add(animation); return animation;
    };
    const originalCreate = doc.createElement;
    doc.createElement = tag => {
        const el = originalCreate(tag); el.open = false; el.scrollTop = 0; el.scrollLeft = 0;
        el.style.setProperty = function(key, value) { this[key] = value; };
        return el;
    };
    doc.createElementNS = (_, tag) => doc.createElement(tag);
    doc.documentElement = doc.body; doc.hidden = false;
    const c = vm.createContext({console, document: doc, currentModule: 'dashboard', currentSite: null, lastData: null,
        esc: value => String(value == null ? '' : value).replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch])),
        localStorage: {getItem: () => null, setItem() {}},
        addEventListener(type, listener) { (events[type] ||= []).push(listener); },
        removeEventListener(type, listener) { events[type] = (events[type] || []).filter(fn => fn !== listener); },
        requestAnimationFrame(fn) { const id = ++frameID; frames.set(id, fn); return id; },
        cancelAnimationFrame(id) { frames.delete(id); },
        matchMedia() { return {matches: false, addEventListener() {}}; },
        ResizeObserver: class { constructor(fn) { this.fn = fn; } observe(el) { this.el = el; observers.add(this); } disconnect() { observers.delete(this); } },
    });
    c.window = c;
    const mount = (id, parent = doc.body, tag = 'div') => { const el = doc.createElement(tag); el.id = id; parent.appendChild(el); return el; };
    const dashboard = mount('mod-dashboard');
    for (const id of ['visual-availability', 'visual-observation-meta', 'visual-queue-trend', 'workbench-kpis', 'workbench-issues', 'workbench-sites', 'workbench-scope', 'workbench-coverage', 'workbench-issue-count', 'workbench-freshness']) mount(id, dashboard);
    const analysis = mount('workbench-analysis', dashboard, 'details'), resources = mount('workbench-resources', dashboard, 'details');
    for (const id of ['visual-stage-bars', 'visual-results']) mount(id, analysis);
    for (const key of ['cpu', 'mem', 'disk']) for (const suffix of ['', '-value', '-stamp']) mount('visual-' + key + suffix, resources);
    for (const name of scripts) controllers.run(fs.readFileSync(path.join(oaoDir, name), 'utf8'), c, {filename: name});
    for (const name of ['workbench.js', 'visual-state.js', 'visuals.js']) controllers.run(fs.readFileSync(path.join(staticDir, name), 'utf8'), c, {filename: name});
    const emit = (type, event = {}) => { for (const listener of [...events[type] || []]) listener(event); };
    const visibility = hidden => { doc.hidden = hidden; for (const listener of [...doc.listeners.visibilitychange || []]) listener(); };
    const toggle = (el, open) => { el.open = open; el.dispatchEvent({type: 'toggle'}); };
    const flush = () => { const batch = [...frames.values()]; frames.clear(); batch.forEach(fn => fn()); };
    return {c, doc, get: id => doc.getElementById(id), analysis, resources, emit, visibility, toggle, flush, frames, observers, animations};
}

const base = Date.parse('2026-11-07T01:00:00Z');
function snap(n = 0, extra = {}) {
    return {instance_id: 'one', generated_at: new Date(base + n).toISOString(), sites: {a: {breaker_paused: false}}, breakers: [], queues: {},
        stages: {a: {site: 'a', stage: 'detail', queue: {queue_len: n / 1000, in_progress: 8, completed: 12, failed: 0}}},
        system: {cpu_available: true, cpu_percent: 42, cpu_sampled_at: new Date(base + n).toISOString()}, ...extra};
}
function show(env, data = snap()) { env.c.acceptMonitorObservation(data); env.c.renderMonitorVisuals(data); }
let passed = 0, failed = 0;
function test(name, body) {
    const env = setup();
    try { body(env); passed++; console.log('PASS ' + name); }
    catch (error) { failed++; console.error('FAIL ' + name + '\n' + error.stack); }
    finally { env.c.destroyMonitorVisuals(); env.c.Oao.motion.cancelAll(); }
}

test('published charts preserve keyboard tokens, table state and bounded observations across polls', env => {
    const {c, doc, get} = env; show(env); env.toggle(env.analysis, true); env.toggle(env.resources, true);
    assert.equal(env.observers.size, 6); assert.equal(doc.querySelector('.oao-chart-error'), null);
    let plot = get('visual-queue-trend').querySelector('.oao-chart-plot'); plot.focus(); plot.dispatchEvent({type: 'keydown', key: 'ArrowRight'});
    const token = doc.activeElement.getAttribute('data-oao-point'); assert.ok(token);
    show(env, snap(3000)); assert.equal(doc.activeElement.getAttribute('data-oao-point'), token);
    let chart = get('visual-stage-bars'), details = chart.querySelector('details'), scroll = chart.querySelector('.oao-chart-table-scroll');
    details.open = true; scroll.scrollTop = 77; scroll.scrollLeft = 19; scroll.focus();
    show(env, snap(6000));
    assert.equal(chart.querySelector('details').open, true); assert.equal(doc.activeElement, chart.querySelector('.oao-chart-table-scroll'));
    assert.equal(doc.activeElement.scrollTop, 77); assert.equal(doc.activeElement.scrollLeft, 19);
    c.currentSite = 'absent'; c.renderMonitorVisuals(c.getVisualSnapshot());
    assert.equal(c.MonitorObservations.size(), 3); assert.match(chart.textContent, /当前范围没有活跃阶段/);
    assert.equal(doc.querySelector('.oao-chart-error'), null);
    c.currentSite = null;
    for (let n = 3; n < 400; n++) c.acceptMonitorObservation(snap(n * 3000, {stages: n % 2 ? {} : snap().stages}));
    c.renderMonitorVisuals(c.getVisualSnapshot());
    assert.equal(doc.querySelector('.oao-chart-error'), null, 'break markers exceeded the released 300-point chart contract');
    assert.ok(c.MonitorObservations.size() <= 240);
});

test('collapse, background and navigation release real observers, frames and animations', env => {
    show(env); env.toggle(env.analysis, true); env.toggle(env.resources, true);
    for (const observer of env.observers) observer.fn(); assert.ok(env.frames.size > 0);
    env.toggle(env.analysis, false); env.toggle(env.resources, false); assert.equal(env.observers.size, 1);
    env.visibility(true); assert.equal(env.animations.size, 0);
    env.visibility(false); env.flush();
    env.c.currentModule = 'settings'; env.c.destroyMonitorVisuals();
    assert.equal(env.observers.size, 0); assert.equal(env.frames.size, 0); assert.equal(env.animations.size, 0);
    assert.equal(env.doc.listeners.visibilitychange.length, 2, 'chart document listeners survived destroy');
});

test('instance restart and missing metadata never mix old observations with new snapshot charts', env => {
    const {c, doc, get} = env;
    show(env); env.toggle(env.analysis, true); env.toggle(env.resources, true); show(env, snap(3000));
    const oldPoint = get('visual-queue-trend').querySelector('[data-oao-point]'); oldPoint.focus();
    show(env, snap(6000, {instance_id: 'restarted'}));
    assert.equal(c.MonitorObservations.size(), 1);
    assert.equal(get('visual-queue-trend').querySelectorAll('[data-oao-point]').length, 2);
    assert.equal(doc.activeElement, get('visual-queue-trend').querySelector('.oao-chart-plot'), 'removed historical point retained focus');
    show(env, {stages: snap(9000).stages});
    assert.equal(c.MonitorObservations.size(), 0);
    assert.match(get('visual-queue-trend').textContent, /无法建立可信趋势/);
    assert.equal(get('visual-stage-bars').querySelector('.oao-chart-error'), null);
    assert.equal(get('visual-cpu-value').textContent, '—');
    c.resetMonitorObservations(); assert.equal(env.observers.size, 0);
    for (const key of ['cpu', 'mem', 'disk']) assert.equal(get('visual-' + key + '-value').textContent, '—');
});

test('resource failures and reconnect gaps reach the published chart as missing points', env => {
    const {c, get} = env; show(env); env.toggle(env.resources, true);
    show(env, snap(3000, {system: {...snap().system, cpu_available: false}}));
    assert.equal(get('visual-cpu-value').textContent, '—'); assert.match(get('visual-cpu-stamp').textContent, /采样失败/);
    show(env, snap(6000)); show(env, snap(30000));
    const points = c.MonitorObservations.resourceTrend('cpu');
    assert.ok(points.some(point => point.x === base + 3000 && point.y === null));
    assert.ok(points.some(point => point.x > base + 6000 && point.x < base + 30000 && point.y === null));
    assert.equal(get('visual-cpu').querySelector('.oao-chart-error'), null);
    assert.equal(get('visual-cpu-value').textContent, '42.0%');
});

test('pagehide cannot be undone by late error renders, then explicit pageshow resume redraws accepted data', env => {
    show(env); env.toggle(env.analysis, true); env.toggle(env.resources, true);
    env.emit('pagehide'); assert.equal(env.observers.size, 0);
    env.c.setMonitorVisualConnection(false); env.c.renderMonitorVisuals(env.c.getVisualSnapshot());
    assert.equal(env.observers.size, 0, 'pagehide was undone by a late status/render callback');
    env.c.resumeMonitorVisuals(); assert.equal(env.observers.size, 6); assert.equal(env.c.MonitorObservations.size(), 1);
    env.emit('pagehide'); env.c.resetMonitorObservations(); env.c.resumeMonitorVisuals();
    assert.equal(env.observers.size, 0, 'credential reset restored old charts on pageshow');
});

test('KPI replacement and hidden modules cancel retired elements instead of animating invisible content', env => {
    const {c, get} = env; c.renderWorkbench(snap());
    const old = get('workbench-kpis').querySelectorAll('strong'); assert.ok(old.some(el => [...env.animations].some(a => a.el === el)));
    c.renderWorkbench(snap(3000));
    assert.ok(old.every(el => ![...env.animations].some(a => a.el === el)), 'detached KPI nodes retained active animations');
    c.destroyMonitorVisuals(); assert.equal(env.animations.size, 0, 'navigation retained KPI animations');
    c.currentModule = 'settings'; c.renderWorkbench(snap(6000)); assert.equal(env.animations.size, 0, 'polling animated a hidden dashboard');
});

test('site summary polling keeps horizontal position and restores keyboard focus without page scrolling', env => {
    const {c, get, doc} = env; c.renderWorkbench(snap());
    const root = get('workbench-sites'), button = root.querySelector('[data-workbench-action="tasks"]');
    root.querySelector('.workbench-scroll').scrollLeft = 180; button.focus();
    c.renderWorkbench(snap(3000));
    assert.equal(root.querySelector('.workbench-scroll').scrollLeft, 180, 'polling reset the site table horizontal scroll');
    assert.equal(doc.activeElement.dataset.workbenchAction, 'tasks');
    assert.equal(doc.activeElement.focusOptions.preventScroll, true);
});

console.log('linked visuals: ' + passed + ' passed, ' + failed + ' failed');
if (failed) process.exitCode = 1;
