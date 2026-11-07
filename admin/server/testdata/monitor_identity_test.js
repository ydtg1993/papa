const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const dir=process.argv[2], source=name=>fs.readFileSync(path.join(dir,name),'utf8');
const requests=[], timers=new Map(), events={}, nodes={};let tid=0,cleans=0,tabDisposals=0;
const values=new Map([['papa_monitor_key','old']]);
const localStorage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
const node=id=>nodes[id]||(nodes[id]={textContent:'old identity',innerHTML:'old identity',classList:{add(){},remove(){},toggle(){}},style:{},focus(){},setAttribute(){},replaceChildren(){this.innerHTML='';}});
const c=vm.createContext({AbortController,Response,localStorage,window:{addEventListener:(k,fn)=>events[k]=fn},
    document:{hidden:false,addEventListener:(k,fn)=>events[k]=fn,getElementById:node,querySelectorAll:()=>[],documentElement:{getAttribute:()=> '1',classList:{add(){},remove(){}}}},
    setTimeout:(fn,ms)=>{timers.set(++tid,{fn,ms});return tid;},clearTimeout:id=>timers.delete(id),
    fetch:(url,opts)=>new Promise(resolve=>requests.push({url,opts,resolve})),renderNav(){},renderAll(data){c.lastData=data;},
    Papa:{leave:()=>{cleans++;return true;}},Oao:{destroy:()=>tabDisposals++},Toast:{err(){}},lastData:null,currentSite:null,
    workbenchConnection:{updatedAt:'old',failed:false,message:''}});
vm.runInContext(source('visual-state.js'),c);
c.resetMonitorObservations=()=>c.MonitorObservations.reset();
vm.runInContext(source('requests.js'),c);
const mo=source('mo.js');vm.runInContext(mo.slice(mo.indexOf('    var KEY_STORAGE'),mo.indexOf('    /* ============ 表格')),c);
const data={instance_id:'same-server',generated_at:'2026-11-07T00:00:00Z',stages:{}};
c.acceptMonitorObservation=d=>c.MonitorObservations.accept(d);
const tick=()=>new Promise(resolve=>setImmediate(resolve));
(async()=>{
    c.MonitorObservations.accept(data);c.lastData=data;c.currentSite='old-site';
    c.startPolling();assert.equal(requests.length,1);
    localStorage.setItem('papa_monitor_key','new');
    assert.equal(typeof events.storage,'function','cross-tab credential changes need a storage listener');
    events.storage({key:'papa_monitor_key',storageArea:localStorage});
    assert.equal(c.MonitorObservations.size(),0,'old identity observations survived external credential rotation');
    assert.equal(c.lastData,null);assert.equal(c.currentSite,null);assert.equal(requests[0].opts.signal.aborted,true);
    assert.equal(requests.length,2,'credential rotation must restart polling instead of losing its timer');
    assert.equal(requests[1].opts.headers.Authorization,'Bearer new');assert.ok(cleans>0&&tabDisposals>0);
    requests[0].resolve({status:401});await tick();assert.equal(localStorage.getItem('papa_monitor_key'),'new');
    requests[1].resolve({ok:true,status:200,json:async()=>({...data,generated_at:'2026-11-07T00:00:01Z'})});await tick();
    assert.equal(c.MonitorObservations.size(),1);assert.ok([...timers.values()].some(t=>t.ms===3000),'polling did not resume');
    const generation=c.authGeneration,count=requests.length;
    events.storage({key:'papa_monitor_key',storageArea:{}});events.storage({key:'unrelated',storageArea:localStorage});
    assert.equal(c.authGeneration,generation);assert.equal(requests.length,count);
    localStorage.removeItem('papa_monitor_key');events.storage({key:null,storageArea:localStorage});
    assert.equal(c.MonitorObservations.size(),0);assert.equal(c.monitorPoll.enabled,false);assert.equal(c.lastData,null);
    assert.equal(c.workbenchConnection.updatedAt,null,'old freshness survived identity reset');
    // Old oao has no destroy API or stale response guard. Its pending render
    // may finish, but only against a detached node. A stale wrapper rejection
    // must not replace the next table in either dependency version.
    vm.runInContext(mo.slice(mo.indexOf('    async function renderTableView'),mo.indexOf('    /** 按 Group',mo.indexOf('    async function renderTableView'))),c);
    c.currentModule='tables';c.tableFilterOpts=()=>({});c.esc=String;
    function tableNode(){return {innerHTML:'',cloneNode:tableNode,parentNode:{replaceChild:fresh=>nodes['oao-view']=fresh}};}
    const renders=[];c.Oao={render:view=>new Promise((resolve,reject)=>renders.push({view,resolve,reject}))};
    nodes['oao-view']=tableNode();
    const a=c.renderTableView('a'),b=c.renderTableView('b');
    assert.notEqual(renders[0].view,renders[1].view,'old oao views must be detached per navigation');
    renders[1].view.innerHTML='new table';renders[1].resolve();await b;
    renders[0].view.innerHTML='late old table';renders[0].reject(new Error('old failure'));await a;
    assert.equal(nodes['oao-view'].innerHTML,'new table');
    c.Oao.destroy=()=>{};
    const d=c.renderTableView('d'),e=c.renderTableView('e');
    renders[3].view.innerHTML='latest table';renders[3].resolve();await e;
    renders[2].reject(new Error('old modern failure'));await d;
    assert.equal(nodes['oao-view'].innerHTML,'latest table','stale outer catch replaced next table');
    const f=c.renderTableView('f');c.retireTableView();c.currentModule='dashboard';
    renders[4].reject(null);await f;
    console.log('PASS external credentials clear identity state, cancel old replies and resume polling');
})().catch(e=>{console.error(e);process.exitCode=1;});
