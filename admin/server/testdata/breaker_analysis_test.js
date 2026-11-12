const controllers=require('./controllers');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(process.argv[2], 'utf8');
const deferred = () => { let resolve, reject; const promise = new Promise((a,b)=>{resolve=a;reject=b;}); return {promise,resolve,reject}; };
function setup() {
    let nodes = {}, auth = 0;
    const requests = [], dialogs = [], downloads = [], copies = [];
    const timers = new Map(); let timerId=0;
    const context = vm.createContext({
        setTimeout(fn,delay) {assert.equal(delay,3000);timers.set(++timerId,fn);return timerId;},
        clearTimeout(id) {timers.delete(id);},
        encodeURIComponent,
        esc: s => String(s).replaceAll('<','&lt;').replaceAll('>','&gt;'),
        captureMonitorView: () => { const generation=auth; return ()=>auth===generation; },
        document: {getElementById: id => nodes[id]},
        Dialog: {open(options) {
            const d=deferred();
            nodes = {};
            for (const id of ['breaker-analysis-status','breaker-analysis-select','breaker-analysis-body']) nodes[id]={textContent:'',innerHTML:'',isConnected:true};
            const pre={textContent:''};nodes['breaker-analysis-body'].querySelector=()=>pre;
            dialogs.push({options,nodes,pre,...d}); return d.promise;
        }},
        apiFetch(url) {const r=deferred();r.url=url;requests.push(r);return r.promise;},
        copyText: s => copies.push(s),
        downloadFile: (url,current)=>{if(current())downloads.push(url);},
    });
    controllers.load(context,source,['breakers/controller']);
    return {requests,dialogs,downloads,copies,timers,load:site=>context.openBreakerAnalysis(site),logout:()=>{auth++;},tick:()=>{const [id,fn]=timers.entries().next().value;timers.delete(id);return fn();}};
}
function response(text, id='one') {return {ok:true,json:async()=>({retention:'bounded history',reports:[{id,at:'time',failures:1,complete:true,summary:'<img onerror=bad>',text,groups:[{stage:'<script>',category:'HTTP 429',count:1,percent:100,tasks:1}]}]})};}
(async()=>{
    const s=setup();const first=s.load('a');const second=s.load('b');
    s.requests[1].resolve(response('<script>raw evidence</script>'));await second;
    s.requests[0].resolve(response('OLD'));await first;
    const latest=s.dialogs[1];
    assert.equal(latest.pre.textContent,'<script>raw evidence</script>');
    assert.ok(!latest.nodes['breaker-analysis-body'].innerHTML.includes('<img'));
    assert.ok(latest.nodes['breaker-analysis-body'].innerHTML.includes('&lt;script&gt;'));
    latest.options.actions[0].onClick();assert.equal(s.copies.length,1);
    latest.options.actions[1].onClick();assert.ok(s.downloads[0].includes('scope=b&id=one&format=text'));
    s.logout();latest.options.actions[0].onClick();latest.options.actions[1].onClick();
    assert.equal(s.copies.length,1);assert.equal(s.downloads.length,1);

    const closed=setup();const pending=closed.load('a');closed.dialogs[0].resolve();await Promise.resolve();
    closed.requests[0].resolve(response('LATE'));await pending;assert.equal(closed.dialogs[0].pre.textContent,'');

    const empty=setup();const p=empty.load('a');empty.requests[0].resolve({ok:true,json:async()=>({retention:'bounded',reports:[]})});await p;
    assert.ok(empty.dialogs[0].nodes['breaker-analysis-status'].textContent.includes('当前没有记录'));

    const failed=setup();const f=failed.load('a');failed.requests[0].resolve({ok:false,text:async()=>'该熔断现场已过期'});await f;
    assert.equal(failed.dialogs[0].nodes['breaker-analysis-status'].textContent,'该熔断现场已过期');

    const live=setup();const open=live.load('a');
    const one=(await response('first','one').json()).reports[0], two=(await response('history','two').json()).reports[0];
    one.lifecycle={state:'manual',recheck_at:'2026-11-12T12:00:00Z',cycle:2,timeline:[{action:'process_restart',reason:'重新启动',check:{verification:'readonly',coverage:'旧覆盖 <unsafe>',sample_ids:['task:1'],verification_note:'旧证明不复用'}}],checks:[
        {stage:'read',state:'passed',verification:'readonly',coverage:'完整解析 <unsafe>',verification_note:'完整只读验证通过',probe_successes:2,task_successes:0,sample_ids:['task:1']},
        {stage:'write',state:'passed',verification:'task',coverage:'完整真实任务路径',verification_note:'未验证：仍需本轮真实任务成功',probe_successes:1,task_successes:0}
    ]};
    live.requests[0].resolve({ok:true,json:async()=>({retention:'live',reports:[one,two]})});await open;
    const html=live.dialogs[0].nodes['breaker-analysis-body'].innerHTML;
    assert.ok(html.includes('验证方式与覆盖范围') && html.includes('完整只读验证通过'));
    assert.ok(html.includes('未验证：仍需本轮真实任务成功'));
    assert.ok(html.includes('完整解析 &lt;unsafe&gt;') && !html.includes('<unsafe>'));
    assert.ok(html.includes('下次小时复查') && html.includes('旧覆盖 &lt;unsafe&gt;') && html.includes('task:1'));
    const select=live.dialogs[0].nodes['breaker-analysis-select'];select.value='1';select.onchange();
    assert.equal(live.dialogs[0].pre.textContent,'history');
    const update=live.tick();assert.equal(live.timers.size,0,'no overlapping refresh during request');
    const newest={...one,id:'newest'}, changed={...two,text:'updated history'};
    live.requests[1].resolve({ok:true,json:async()=>({retention:'live',reports:[newest,changed,one]})});await update;
    assert.equal(select.value,'1');assert.equal(live.dialogs[0].pre.textContent,'updated history');
    live.dialogs[0].resolve();await Promise.resolve();assert.equal(live.timers.size,0,'close must cancel refresh');

    assert.equal(s.timers.size,1);await s.tick();assert.equal(s.requests.length,2,'logout must stop refresh requests');
})().catch(e=>{console.error(e);process.exitCode=1;});
