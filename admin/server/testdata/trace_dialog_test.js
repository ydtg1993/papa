const controllers=require('./controllers');
const assert = require('node:assert/strict'), fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const pending = [], dialogs = [];
const context = vm.createContext({ console, authGeneration: 1,
 window: { fetch: async () => ({ ok: true }) },
 apiFetch: (url) => new Promise(resolve => pending.push({ url, resolve })),
 Dialog: { open: options => dialogs.push(options) }, Toast: { err: message => { throw Error(message); } },
 esc: text => String(text).replaceAll('<', '&lt;').replaceAll('>', '&gt;'), downloadFile: async () => {}
});
controllers.run(fs.readFileSync(path.join(process.argv[2], 'trace.js'), 'utf8'), context);
const tick = () => new Promise(resolve => setImmediate(resolve));
(async () => {
 await context.window.fetch('/api/oao/task/action/trace', { method: 'POST', body: JSON.stringify({ id: '7' }) });
 assert.equal(pending[0].url, '/api/task/trace?id=7');
 pending[0].resolve({ json: async () => ({ steps: [] }) }); await tick();
 assert.equal(dialogs[0].title, '任务追踪 #7');
 assert.match(dialogs[0].body, /没有保留的追踪记录/);
 const first = context.showTaskTrace(8), second = context.showTaskTrace(9);
 pending[2].resolve({ json: async () => ({ steps: [{ attempt: 1, step: '<unsafe>', status: 'failed', message: 'failure', duration: 1000000 }] }) }); await second;
 pending[1].resolve({ json: async () => ({ steps: [] }) }); await first;
 assert.equal(dialogs.length, 2); assert.equal(dialogs[1].title, '任务追踪 #9');
 assert.match(dialogs[1].body, /第 2 次尝试/); assert.match(dialogs[1].body, /&lt;unsafe&gt;/);
 const oldIdentity = context.showTaskTrace(10); context.authGeneration++;
 pending[3].resolve({ json: async () => ({ steps: [] }) }); await oldIdentity;
 assert.equal(dialogs.length, 2);
 console.log('PASS task trace action opens scoped dialog; grouping, escaping, stale task and identity guards');
})().catch(error => { console.error(error); process.exitCode = 1; });

