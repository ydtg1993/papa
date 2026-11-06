const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const source=fs.readFileSync(path.join(process.argv[2],'workbench.js'),'utf8');
const nodes={},calls=[];
const node=()=>({innerHTML:'',textContent:'',dataset:{},classList:{toggle:()=>{}},querySelectorAll:()=>[],contains:()=>false});
const c=vm.createContext({Date,Map,Number,Object,Array,String,
    currentSite:null,currentModule:'dashboard',lastData:null,
    document:{activeElement:null,getElementById:id=>nodes[id]||(nodes[id]=node())},
    esc:s=>String(s==null?'':s).replace(/[&<>"']/g,ch=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch])),
    switchSite:site=>{c.currentSite=site;calls.push(['site',site]);},
    switchModule:name=>{c.currentModule=name;calls.push(['module',name]);},
    openTable:(key,label)=>calls.push(['table',key,label]),
});vm.runInContext(source,c);
const q=(queued,running,completed,failed)=>({queue_len:queued,in_progress:running,completed,failed});
const data={sites:{a:{stage_count:2,breaker_paused:false},b:{stage_count:1,breaker_paused:false},idle:{stage_count:3,breaker_paused:false}},
    stages:{'a:list':{site:'a',stage:'list',queue:q(3,2,10,4)},'b:list':{site:'b',stage:'list',queue:q(5,1,20,0)}},breakers:[],
    queues:{error_queue:{enabled:true,backlog:0,backlog_at:'2026-11-06T01:00:00Z'}}};
{
    const m=c.buildWorkbench(data,null);assert.equal(m.rows.length,3);assert.equal(m.totals.queued,8);assert.equal(m.totals.running,3);
    assert.equal(m.totals.completed,30);assert.equal(m.totals.failed,4);assert.equal(m.totals.activeStages,2);
    assert.equal(m.rows.find(r=>r.site==='idle').activeStages,0);assert.equal(m.issues.length,0,'cumulative failures are not current incidents');
    const a=c.buildWorkbench(data,'a');assert.equal(a.rows.length,1);assert.equal(a.totals.queued,3);
}
{
    const unknown=c.buildWorkbench({sites:{a:{}},stages:{x:{site:'a',queue:{failed:0,queue_len:'bad'}}}},null);
    assert.equal(unknown.totals.queued,null);assert.equal(unknown.totals.running,null);assert.equal(unknown.rows[0].pauseKnown,false);
    const m=c.buildWorkbench({...data,breakers:[{site:'a',stage:'list',paused:true,source:'manual'},{site:'b',paused:true,source:'breaker',reason:'too many errors'}]},null);
    assert.equal(m.issues[0].site,'b');assert.ok(m.rows.find(r=>r.site==='a').state.includes('暂停'));
    assert.equal(c.buildWorkbench({...data,sites:{a:{breaker_paused:true,pause_source:'manual'}},breakers:undefined},'a').rows[0].paused,true);
}
{
    const m=c.buildWorkbench({...data,queues:{error_queue:{backlog:99,backlog_at:'0001-01-01T00:00:00Z'},repeat_queue:{enabled:false,backlog:12,backlog_at:'2026-11-06T01:00:00Z',last_error:'previous error'}}},'a');
    assert.ok(m.issues.some(i=>i.detail.includes('尚未采样')));assert.ok(m.issues.some(i=>i.title.includes('上次执行报错')));
    assert.ok(m.issues.some(i=>i.title.includes('12')));assert.ok(m.issues.every(i=>i.site===null&&i.scope==='全部站点'));
}
{
    const hostile='x"><img src=x onerror=alert(1)>';
    const d={sites:JSON.parse('{"__proto__":{},"constructor":{}}'),stages:{one:{site:hostile,queue:q(0,0,0,0)}},breakers:[{site:hostile,paused:true,source:'breaker',reason:'<script>bad</script>'}]};
    assert.equal(c.buildWorkbench(d,null).rows.length,3);
    c.currentSite=null;c.renderWorkbench(d);const html=Object.values(nodes).map(n=>n.innerHTML).join('');
    assert.ok(!html.includes('<img'));assert.ok(!html.includes('<script>bad'));assert.ok(html.includes('&lt;script&gt;'));
}
{
    c.lastData=data;c.currentModule='dashboard';c.navigateWorkbench('tasks','a');assert.equal(c.currentSite,'a');assert.deepEqual(calls.slice(-2),[['site','a'],['table','task','任务']]);
    c.currentModule='dashboard';c.navigateWorkbench('batches','b');assert.deepEqual(calls.slice(-2),[['site','b'],['module','schedules']]);
    c.currentModule='dashboard';c.navigateWorkbench('queues',null);assert.deepEqual(calls.at(-1),['module','queues']);
    const count=calls.length;c.currentModule='dashboard';c.navigateWorkbench('tasks','missing');assert.equal(calls.length,count);
}
{
    c.setWorkbenchConnection(true,'已连接');const stamp=c.workbenchConnection.updatedAt;
    c.currentSite=null;c.renderWorkbench(data);assert.equal(c.workbenchConnection.updatedAt,stamp);
    c.setWorkbenchConnection(false,'请求超时');assert.ok(nodes['workbench-freshness'].textContent.includes('保留上次快照'));
    c.setWorkbenchConnection(true,'已连接');assert.ok(!nodes['workbench-freshness'].textContent.includes('保留上次快照'));
}
{
    const old={dataset:{workbenchAction:'tasks',site:'a'}},fresh={dataset:old.dataset,focus:()=>c.document.activeElement=fresh};
    const el={innerHTML:'old',contains:n=>n===old,querySelectorAll:s=>s==='[data-workbench-action]'?[fresh]:[]};
    c.document.activeElement=old;c.replaceWorkbenchHTML(el,'new');assert.equal(c.document.activeElement,fresh,'refresh lost keyboard focus');
    let writes=0;Object.defineProperty(el,'innerHTML',{get:()=> 'new',set:()=>writes++});c.replaceWorkbenchHTML(el,'new');assert.equal(writes,0);
}
{
    const old={dataset:{workbenchAction:'queues',workbenchKey:'error_queue:backlog'}};
    const fresh={dataset:old.dataset,focus:()=>c.document.activeElement=fresh},other={dataset:{workbenchAction:'queues',workbenchKey:'repeat_queue:backlog'},focus:()=>c.document.activeElement=other};
    const more={open:true},replacement={open:false};let replaced=false;
    const el={contains:n=>n===old,querySelectorAll:s=>s==='[data-workbench-action]'?[fresh,other]:[replaced?replacement:more]};
    Object.defineProperty(el,'innerHTML',{set:()=>replaced=true});c.document.activeElement=old;c.replaceWorkbenchHTML(el,'changed');
    assert.equal(c.document.activeElement,fresh,'same-label queue button stole focus');assert.equal(replacement.open,true,'refresh collapsed expanded issues');
}
{
    // Production tabs share the union of sites; navigation must not silently
    // reset a stage-only site to the global scope. "__all__" is a valid key.
    const mo=fs.readFileSync(path.join(process.argv[2],'mo.js'),'utf8');
    const a=mo.indexOf('    function renderSiteTabs('),b=mo.indexOf('    function switchSite(',a);vm.runInContext(mo.slice(a,b),c);
    c.siteLabel=String;c.fmtAgo=String;c.currentSite='orphan';
    c.renderSiteTabs({...data,stages:{x:{site:'orphan',queue:q(0,0,0,0)}}});assert.equal(c.currentSite,'orphan');
    const all={dataset:{all:'true'}},named={dataset:{site:'__all__'}};nodes['site-tabs'].querySelectorAll=()=>[all,named];
    c.renderSiteTabs({...data,sites:{__all__:{key:'__all__'}}});named.onclick();assert.equal(c.currentSite,'__all__');all.onclick();assert.equal(c.currentSite,null);
}
