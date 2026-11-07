const assert = require('node:assert/strict'), fs = require('node:fs'), vm = require('node:vm'), path = require('node:path');
const source = fs.readFileSync(path.join(process.argv[2], 'requests.js'), 'utf8');
const deferred = () => { let resolve, reject; const promise = new Promise((a,b) => { resolve=a; reject=b; }); return {promise,resolve,reject}; };
const flush = () => new Promise(resolve => setImmediate(resolve));
const response = value => ({ok:true,status:200,json:async()=>value,text:async()=>JSON.stringify(value)});
function setup() {
    let key = 'old', timerID = 0;
    const requests=[], rendered=[], observations=[], messages=[], timers=new Map(), events={};
    const c=vm.createContext({AbortController, Response, Date, JSON, Math, Object, encodeURIComponent,
        authGeneration:0, loadKey:()=>key,
        setTimeout:(fn,ms)=>{ timers.set(++timerID,{fn,ms}); return timerID; }, clearTimeout:id=>timers.delete(id),
        document:{hidden:false,addEventListener:(n,fn)=>events[n]=fn,getElementById:()=>({textContent:''})},
        window:{addEventListener:(n,fn)=>events[n]=fn},
        fetch:(url,opts)=>{ const d=deferred(); requests.push({...d,url,opts}); return d.promise; },
        acceptMonitorObservation:data=>observations.push(data),renderAll:data=>rendered.push(data), hideLogin:()=>messages.push('hide'), showLogin:()=>messages.push('login'),
        setStatus:(ok,text)=>messages.push(text), Toast:{err:text=>messages.push(text)}, renderNav:()=>{},
    });
    c.clearKey=()=>{key='';c.authGeneration++;c.stopPolling();};
    vm.runInContext(source,c);
    return {c,requests,rendered,observations,messages,timers,events,login:()=>{key='new';c.authGeneration++;}};
}
(async()=>{
    // BFCache freeze can miss storage events. Retire all polling on pagehide,
    // reconcile the credential before restoring visuals, then resume once.
    {const s=setup(),order=[];s.c.startPolling();const first=s.requests[0];
     assert.equal(typeof s.events.pagehide,'function','pagehide must retire monitor polling');
     s.events.pagehide({persisted:true});assert.equal(first.opts.signal.aborted,true);
     first.resolve(response('old'));await flush();assert.equal(s.rendered.length,0);assert.equal(s.observations.length,0);
     const count=s.requests.length;await s.c.fetchData();assert.equal(s.requests.length,count,'hidden page restarted polling');
     s.c.syncMonitorCredential=()=>{order.push('credential');s.login();s.c.startPolling();return true;};
     s.c.resumeMonitorVisuals=()=>order.push('visuals');s.events.pageshow({persisted:true});
     assert.deepEqual(order,['credential','visuals']);assert.equal(s.requests.length,count+1,'restore duplicated monitor polling');
     assert.equal(s.requests.at(-1).opts.headers.Authorization,'Bearer new');s.requests.at(-1).resolve(response('restored'));await flush();
     assert.deepEqual(s.rendered,['restored']);assert.ok([...s.timers.values()].some(t=>t.ms===3000));}
    // Old headers, old bodies, body failures and auth errors cannot replace newer state.
    for(const kind of ['headers','body','body-error','401','network']) {
        const s=setup(), first=s.c.fetchData();
        const body=deferred();
        if(kind.startsWith('body')) {s.requests[0].resolve({ok:true,status:200,json:()=>body.promise});await flush();}
        const second=s.c.fetchData();
        s.requests[1].resolve(response('new'));await second;
        if(kind==='headers') s.requests[0].resolve(response('old'));
        if(kind==='401') s.requests[0].resolve({status:401});
        if(kind==='network') s.requests[0].reject(new Error('old failure'));
        if(kind==='body') body.resolve('old');
        if(kind==='body-error') body.reject(new Error('old body failure'));
        await first;
        assert.deepEqual(s.rendered,['new']);assert.deepEqual(s.observations,['new'],'obsolete response entered observation history');assert.equal(s.messages.includes('login'),false);
    }
    // Credentials change without a second monitor request: old 401 is still obsolete.
    { const s=setup(), done=s.c.fetchData();s.login();s.requests[0].resolve({status:401});await done;assert.equal(s.messages.length,0); }
    // One completed request schedules the next. Failures back off; success resets delay.
    { const s=setup();s.c.monitorPoll.enabled=true;let p=s.c.fetchData();assert.equal([...s.timers.values()].filter(t=>t.ms===3000).length,0);
      s.requests[0].reject(new Error('offline'));await p;assert.equal(s.c.monitorPoll.delay,6000);
      p=s.c.fetchData();s.requests[1].resolve(response({}));await p;assert.equal(s.c.monitorPoll.delay,3000);
      s.c.document.hidden=true;s.events.visibilitychange();assert.equal(s.timers.size,0);await s.c.fetchData();assert.equal(s.requests.length,2);
      s.c.document.hidden=false;s.events.visibilitychange();assert.equal(s.requests.length,3);s.requests[2].resolve({status:401});await flush();assert.equal(s.c.monitorPoll.enabled,false);assert.equal(s.timers.size,0);
    }
    // Timeout covers response body too and never renders late successful JSON.
    {const s=setup(),p=s.c.fetchData(),b=deferred();s.requests[0].resolve({ok:true,status:200,json:()=>b.promise});await flush();
     [...s.timers.values()].find(t=>t.ms===10000).fn();b.resolve('late');await p;assert.deepEqual(s.rendered,[]);assert.deepEqual(s.observations,[]);assert.ok(s.messages.some(m=>m.includes('超时')));}
    // Malformed JSON is handled and schedules a retry, no unhandled rejection.
    {const s=setup(),p=s.c.fetchData();s.requests[0].resolve({ok:true,status:200,json:async()=>{throw new Error('bad json');}});await p;assert.ok(s.messages.includes('bad json'));}
    // Native errors retain actionable text; old-session errors cannot clear credentials.
    {const s=setup();let p=s.c.apiPost('/write',{});s.requests[0].resolve({ok:false,status:409,text:async()=>'版本已变化'});assert.equal(await p,null);assert.ok(s.messages[0].includes('版本已变化'));
     p=s.c.apiFetch('/read');s.login();s.requests[1].resolve({status:401});await p;assert.equal(s.messages.includes('login'),false);}
    // Native success is not complete at headers; body stalls must time out too.
    {const s=setup(),body=deferred();let settled=false;
     const p=s.c.apiPost('/write',{}).then(v=>{settled=true;return v;});
     s.requests[0].resolve({ok:true,status:200,text:()=>body.promise});await flush();
     assert.equal(settled,false,'write resolved before its body');
     [...s.timers.values()].find(t=>t.ms===10000).fn();await flush();
     assert.equal(settled,true,'body deadline did not release request');assert.equal(await p,null);
     assert.ok(s.messages.some(m=>m.includes('结果尚未确认')));assert.equal(s.requests[0].opts.signal.aborted,true);}
    // A session change while reading the body discards the old success.
    {const s=setup(),body=deferred(),p=s.c.apiFetch('/read');s.requests[0].resolve({ok:true,status:200,text:()=>body.promise});await flush();
     s.login();body.resolve('{}');assert.equal(await p,null);assert.equal(s.messages.length,0);}
    // Test the actual dependency's request function: stale table 401 must not
    // log out a new session, and no write may be automatically replayed.
    {const s=setup(),oao=fs.readFileSync(path.join(process.argv[2],'oao.js'),'utf8');
     const a=oao.indexOf('    async function request('),b=oao.indexOf('    async function api(',a);
     const mo=fs.readFileSync(path.join(process.argv[2],'mo.js'),'utf8'),init=mo.indexOf('    Oao.init({',mo.indexOf('// oao 表格组件：'));
     s.c.Oao={init:opts=>{s.c.authHeaders=opts.headers;s.c.onUnauthorized=opts.onUnauthorized;}};
     vm.runInContext(mo.slice(init,mo.indexOf('    });',init)+7),s.c);
     vm.runInContext(oao.slice(a,b),s.c);
     const old=s.c.request('/api/oao/task/action/retry',{method:'POST',body:'{}'}).catch(e=>e);
     s.login();s.requests[0].resolve({status:401});await flush();
     assert.equal(s.requests.length,2,'401 must verify the current credential');assert.equal(s.requests[1].opts.headers.Authorization,'Bearer new');
     s.requests[1].resolve(response({}));await old;assert.equal(s.c.loadKey(),'new');assert.equal(s.requests.filter(r=>r.opts.method==='POST').length,1);
     const first=s.c.handleTableUnauthorized(),second=s.c.handleTableUnauthorized();assert.equal(s.requests.length,3,'same-session probes must coalesce');
     s.requests[2].resolve({status:401});await Promise.all([first,second]);assert.equal(s.c.loadKey(),'');assert.ok(s.messages.includes('login'));}
    // Publication's raw status handling still uses the common deadline and
    // authentication guard, and must not turn an error into an implicit retry.
    {const s=setup(),p=s.c.apiRequest('/api/rules/change',{method:'POST',body:'{}',rawHTTPError:true});
     s.requests[0].resolve(new Response('stale',{status:409,headers:{'X-Rule-Outcome':'rejected'}}));
     const response=await p;assert.equal(response.status,409);assert.equal(response.headers.get('X-Rule-Outcome'),'rejected');assert.equal(s.requests.length,1);assert.equal(s.messages.length,0);}
    // Queue double click dispatches once and releases its lock on invalid JSON.
    {const mo=fs.readFileSync(path.join(process.argv[2],'mo.js'),'utf8'),s=setup(),nodes={textContent:''};
     s.c.queueMeta=()=>({label:'轮询队列',url:'/repeat',done:'处理',key:'count'});s.c.document.getElementById=()=>nodes;s.c.document.querySelectorAll=()=>[];
     s.c.Toast.ok=()=>{};s.c.fetchData=()=>{};
     const a=mo.indexOf('    var queueFlights ='),b=mo.indexOf('    /* ============ 站点 Tab',a);assert.ok(a>0&&b>a);vm.runInContext(mo.slice(a,b),s.c);
     const p=s.c.triggerQueue('repeat');await s.c.triggerQueue('repeat');assert.equal(s.requests.length,1);s.requests[0].resolve({ok:true,status:200,text:async()=>'{broken'});await p;assert.equal(s.c.queueFlights.repeat,undefined);assert.ok(s.messages.some(m=>m.includes('结果尚未确认')));}
    // Trace responses are ordered from the click, not from arrival of headers.
    for (const mode of ['headers','body','401','body-error']) {
        const s=setup(),dialogs=[];s.c.window=s.c;s.c.esc=String;s.c.Dialog={open:d=>dialogs.push(d)};
        vm.runInContext(fs.readFileSync(path.join(process.argv[2],'trace.js'),'utf8'),s.c);
        await (async()=>{const p=s.c.fetch('/api/oao/task/action/trace',{method:'POST',body:'{"id":"1"}'});s.requests[0].resolve(response({}));await p;})();
        await flush();assert.equal(s.requests[1].url,'/api/task/trace?id=1');
        const body=deferred();if(mode.startsWith('body')){s.requests[1].resolve({ok:true,status:200,text:()=>body.promise});await flush();}
        if(mode==='401')s.login();
        else {
            const p=s.c.fetch('/api/oao/task/action/trace',{method:'POST',body:'{"id":"2"}'});s.requests[2].resolve(response({}));await p;await flush();
            s.requests[3].resolve(response({steps:[]}));await flush();
        }
        if(mode==='headers')s.requests[1].resolve(response({steps:[]}));
        if(mode==='401')s.requests[1].resolve({status:401});
        if(mode==='body')body.resolve('{"steps":[]}');
        if(mode==='body-error')body.reject(new Error('old trace body'));
        await flush();assert.deepEqual(dialogs.map(d=>d.title),mode==='401'?[]:['任务追踪 #2']);assert.equal(s.messages.includes('login'),false);
    }
    // Native quality panels open Trace with GET only; leaving the panel cancels display.
    {const s=setup(),dialogs=[];s.c.window=s.c;s.c.esc=String;s.c.Dialog={open:d=>dialogs.push(d)};
     vm.runInContext(fs.readFileSync(path.join(process.argv[2],'trace.js'),'utf8'),s.c);
     let current=true;const p=s.c.showTaskTrace('9007199254740993',()=>current);
     assert.equal(s.requests.length,1);assert.equal(s.requests[0].url,'/api/task/trace?id=9007199254740993');
     current=false;s.requests[0].resolve(response({steps:[]}));await p;assert.equal(dialogs.length,0);}
    // Successful re-authentication reloads a batch page that lost its old request.
    {const s=setup(),mo=fs.readFileSync(path.join(process.argv[2],'mo.js'),'utf8');
     const a=mo.indexOf('    var pendingLoginRefresh ='),b=mo.indexOf('    function setStatus(',a);vm.runInContext(mo.slice(a,b),s.c);
     let loads=0;s.c.currentModule='schedules';s.c.loadSchedules=()=>loads++;s.c.pendingLoginRefresh=true;
     const p=s.c.fetchData();s.requests[0].resolve(response({}));await p;assert.equal(loads,1);
     const q=s.c.fetchData();s.requests[1].resolve(response({}));await q;assert.equal(loads,1);}
    // Late menu failures from a previous login cannot clear a new menu.
    {const s=setup(),mo=fs.readFileSync(path.join(process.argv[2],'mo.js'),'utf8'),lists=[],nav={innerHTML:'',querySelectorAll:()=>[]};
     s.c.setMonitorNavigation=(tables,pages)=>{nav.innerHTML=tables.concat(pages).map(item=>item.label).join(' ');};
     s.c.Oao={list:()=>{const d=deferred();lists.push(d);return d.promise;}};
     const a=mo.indexOf('    var navRequestSeq ='),b=mo.indexOf('    /* ============ 自定义页',a);vm.runInContext(mo.slice(a,b),s.c);
     const p=s.c.renderNav();s.login();const q=s.c.renderNav();lists[1].resolve([{key:'new',label:'new menu'}]);await flush();
     s.requests[0].resolve(response({pages:[]}));await q;assert.ok(nav.innerHTML.includes('new menu'));
     lists[0].reject(new Error('old unauthorized'));await p;assert.ok(nav.innerHTML.includes('new menu'));assert.equal(s.requests.length,1);}
})().catch(e=>{console.error(e);process.exitCode=1;});
