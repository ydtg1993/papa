const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(process.argv[2], 'utf8');
const start = source.indexOf('var breakerAnalysisSeq = 0;');
const end = source.indexOf('// 暂停抓取：', start);
assert.ok(start >= 0 && end > start);
const deferred = () => { let resolve, reject; const promise = new Promise((a,b)=>{resolve=a;reject=b;}); return {promise,resolve,reject}; };
function setup() {
    let nodes = {}, auth = 0;
    const requests = [], dialogs = [], downloads = [], copies = [];
    const context = vm.createContext({
        encodeURIComponent,
        esc: s => String(s).replaceAll('<','&lt;').replaceAll('>','&gt;'),
        captureBreakerAnalysisView: () => { const generation=auth; return ()=>auth===generation; },
        document: {getElementById: id => nodes[id]},
        Dialog: {close(){},open(options) {
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
    vm.runInContext(source.slice(start,end),context);
    return {requests,dialogs,downloads,copies,load:site=>context.openBreakerAnalysis(site),logout:()=>{auth++;}};
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
})().catch(e=>{console.error(e);process.exitCode=1;});
