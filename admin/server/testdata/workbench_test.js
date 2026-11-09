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
    const before = calls.length;c.navigateWorkbench('batches','b');assert.equal(calls.length,before,'removed batch navigation must do nothing');
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
    const el={contains:n=>n===old,querySelectorAll:s=>s==='[data-workbench-action]'?[fresh,other]:[]};
    c.document.activeElement=old;c.replaceWorkbenchHTML(el,'changed');
    assert.equal(c.document.activeElement,fresh,'same-label queue button stole focus');
}
{
    const pauses=Array.from({length:5},(_,i)=>({site:'paused-'+i,paused:true,source:'breaker',reason:'pause '+i}));
    const mixed={...data,breakers:pauses,queues:{error_queue:{last_error:'queue execution failed',backlog:9,backlog_at:'2026-11-06T01:00:00Z'}}};
    c.currentSite=null;c.renderWorkbench(mixed);const html=nodes['workbench-issues'].innerHTML;
    assert.equal((html.match(/<article /g)||[]).length,3,'overview must show at most three issues');
    assert.equal(nodes['workbench-issue-count'].textContent,7);
    assert.ok(!html.includes('<details'),'the remaining issues were hidden in another long disclosure');
    assert.match(html,/查看队列异常/);assert.match(html,/查看暂停详情/);
    assert.ok(!html.includes('查看全部异常'),'queue navigation misrepresents pause coverage');
    c.renderWorkbench({...mixed,breakers:pauses.slice(0,3)});
    assert.match(nodes['workbench-issues'].innerHTML,/查看队列异常/);assert.ok(!nodes['workbench-issues'].innerHTML.includes('查看暂停详情'));
    c.renderWorkbench({...mixed,queues:undefined});
    assert.match(nodes['workbench-issues'].innerHTML,/治理队列监控未提供/,'overflow hid an unavailable monitor field');
    assert.ok(!nodes['workbench-issues'].innerHTML.includes('查看队列异常'));
    c.renderWorkbench({...data,breakers:[{paused:true,source:'manual',reason:'global pause'}]});
    assert.match(nodes['workbench-issues'].innerHTML,/data-workbench-action="pauses"/,'global pause has no reachable details');
}
{
    // Use the production bell handler; a repeat click must keep the panel
    // open, and the same click must not bubble into the outside-close handler.
    const mo=fs.readFileSync(path.join(process.argv[2],'mo.js'),'utf8');
    const a=mo.indexOf('    function toggleBell('),b=mo.indexOf('    function closeBell(',a);vm.runInContext(mo.slice(a,b),c);
    let focused=0,stopped=0;nodes['bell-panel']={hidden:true};nodes.bell={setAttribute:(key,value)=>nodes.bell[key]=value,focus:()=>focused++};
    c.currentModule='dashboard';c.navigateWorkbench('pauses',null,{stopPropagation:()=>stopped++});
    assert.equal(nodes['bell-panel'].hidden,false);assert.equal(nodes.bell['aria-expanded'],'true');assert.equal(focused,1);assert.equal(stopped,1);
    c.navigateWorkbench('pauses',null,{stopPropagation:()=>stopped++});assert.equal(nodes['bell-panel'].hidden,false);assert.equal(stopped,2);
    let rendered;c.renderBell=list=>rendered=list;
    c.lastData={sites:{legacy:{breaker_paused:true,pause_source:'manual',pause_reason:'legacy pause'}}};c.navigateWorkbench('pauses');
    assert.equal(rendered[0].site,'legacy');assert.equal(rendered[0].reason,'legacy pause','pause details dropped the supported older snapshot shape');
    c.lastData=data;
    c.currentModule='settings';nodes['bell-panel'].hidden=true;c.navigateWorkbench('pauses');assert.equal(nodes['bell-panel'].hidden,true);
    c.currentModule='dashboard';
}
{
    // Production tabs share the union of sites; navigation must not silently
    // reset a stage-only site to the global scope. "__all__" is a valid key.
    const mo=fs.readFileSync(path.join(process.argv[2],'mo.js'),'utf8');
    const a=mo.indexOf('    function renderSiteTabs('),b=mo.indexOf('    function switchSite(',a);vm.runInContext(mo.slice(a,b),c);
    c.siteLabel=String;c.fmtAgo=String;c.currentSite='orphan';
    vm.runInContext(mo.slice(mo.indexOf('    function refreshSiteModule('),mo.indexOf('    var lastData =',mo.indexOf('    function refreshSiteModule('))),c);
    c.renderSiteTabs({...data,stages:{x:{site:'orphan',queue:q(0,0,0,0)}}});assert.equal(c.currentSite,'orphan');
    const all={dataset:{all:'true'}},named={dataset:{site:'__all__'}};nodes['site-tabs'].querySelectorAll=()=>[all,named];
    c.renderSiteTabs({...data,sites:{__all__:{key:'__all__'}}});named.onclick();assert.equal(c.currentSite,'__all__');all.onclick();assert.equal(c.currentSite,null);
}

// Polls must not rebuild tabs simply because the browser serialized aria-pressed.
{
    c.currentSite=null; const el=nodes['site-tabs']; let writes=0;
    Object.defineProperty(el,'innerHTML',{configurable:true,get:()=>'<serialized with aria attributes>',set:()=>writes++});
    c.renderSiteTabs(data);writes=0;c.renderSiteTabs(data);assert.equal(writes,0,'unchanged site buttons were rebuilt');
}

// A scope that disappears with the last site must not leave a hidden filter.
{
    c.currentSite='gone';c.renderSiteTabs({stages:{},sites:{},breakers:[]});
    assert.equal(c.currentSite,null,'last site vanished but the hidden selection survived');
}
{
    c.currentModule='tables';c.currentTable='tasks';c.currentSite='gone';
    let reloaded=0;c.renderTableView=key=>{assert.equal(key,'tasks');assert.equal(c.currentSite,null);reloaded++;};
    c.renderSiteTabs({stages:{},sites:{},breakers:[]});assert.equal(reloaded,1,'vanished site left stale table filters');
    c.currentModule='dashboard';
}
