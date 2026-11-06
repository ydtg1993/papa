const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const source=fs.readFileSync(path.join(process.argv[2],'schedules.js'),'utf8');
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {resolve,promise};};
function setup(){
    const nodes={},requests=[],posts=[],confirms=[];
    const c=vm.createContext({Date,encodeURIComponent,authGeneration:0,currentModule:'schedules',currentSite:null,
        document:{getElementById:id=>nodes[id]||(nodes[id]={innerHTML:'',textContent:'',checked:false,querySelectorAll:()=>[]})},
        esc:s=>String(s==null?'':s).replace(/</g,'&lt;'),
        apiFetch:(url,opts)=>{const d=deferred();requests.push({...d,url,opts});return d.promise;},
        apiPost:(url,body)=>{posts.push({url,body});return Promise.resolve({ok:true});},Toast:{ok:()=>{}},
        Dialog:{confirm:()=>{const d=deferred();confirms.push(d);return d.promise;}}
    });vm.runInContext(source,c);return {c,nodes,requests,posts,confirms};
}
const detail=id=>({control:{id,site:'site',stage:'schedule',status:2,repeatable:1,reprocess:2},batch:{BatchID:'a'.repeat(64),RootID:7,Sequence:1,Total:2,Pending:0,Processing:0,Success:1,Failed:1},rules:{detail:{id:'r',version:'v1'}},members:[{id:'7',stage:'detail',status:3,reprocess:4}],next:'0'});
const resp=x=>({json:async()=>x});
(async()=>{
    {const s=setup();s.c.scheduleView.id='1';const p=s.c.loadScheduleDetail('');s.c.scheduleView.id='2';const q=s.c.loadScheduleDetail('');s.requests[1].resolve(resp(detail('2')));await q;s.requests[0].resolve(resp(detail('1')));await p;assert.equal(s.c.scheduleView.data.control.id,'2');assert.ok(s.nodes['schedule-detail-body'].innerHTML.includes('失败阻塞'));}
    for(const mutate of [c=>c.authGeneration++,c=>{c.currentModule='dashboard';c.invalidateSchedules();}]) {const s=setup(),p=s.c.loadSchedules(true);mutate(s.c);s.requests[0].resolve(resp({items:[],next:'0'}));await p;assert.ok(s.nodes['schedule-list'].innerHTML.includes('正在读取'));}
    {const s=setup();s.c.scheduleView.id='1';s.c.scheduleView.data=detail('1');s.c.scheduleView.data.next='50';s.c.pageScheduleMembers(true);assert.ok(s.requests[0].url.includes('after=50'));assert.ok(s.requests[0].url.includes('batch='+'a'.repeat(64)));s.requests[0].resolve(resp(detail('1')));}
    // Confirmation is single flight; a changed view invalidates pending permission.
    {const s=setup();s.c.scheduleView.id='1';s.c.scheduleView.data=detail('1');const p=s.c.scheduleTaskAction('retry','7');await s.c.scheduleTaskAction('retry','7');assert.equal(s.confirms.length,1);s.c.currentModule='dashboard';s.confirms[0].resolve(true);await p;assert.equal(s.posts.length,0);assert.equal(s.c.scheduleView.busy,false);}
    // Retry carries exact task/version, and reloads current detail after success.
    {const s=setup();s.c.scheduleView.id='1';s.c.scheduleView.data=detail('1');const p=s.c.scheduleTaskAction('retry','7');s.confirms[0].resolve(true);await p;assert.equal(s.posts.length,1);assert.equal(s.posts[0].body.row.reprocess,4);assert.equal(s.posts[0].body.id,'7');assert.equal(s.requests.length,1);s.requests[0].resolve(resp(detail('1')));}
    // A newly selected detail rendered during a pending write must be unlocked
    // when the old write ends, without refreshing the new controller by mistake.
    {const s=setup(),write=deferred();s.c.apiPost=()=>write.promise;s.c.scheduleView.id='1';s.c.scheduleView.data=detail('1');
     const p=s.c.scheduleTaskAction('retry','7');s.confirms[0].resolve(true);await new Promise(r=>setImmediate(r));
     s.c.scheduleView.id='2';s.c.scheduleView.data=detail('2');s.c.renderScheduleDetail(s.c.document.getElementById('schedule-detail-body'),s.c.scheduleView.data);
     assert.ok(s.nodes['schedule-detail-body'].innerHTML.includes(' disabled'));
     write.resolve({ok:true});await p;assert.equal(s.requests.length,0);assert.equal(s.nodes['schedule-detail-body'].innerHTML.includes(' disabled'),false);}
    // A refreshed control snapshot also updates the list row (e.g. pause).
    {const s=setup();s.c.scheduleView.id='1';s.c.scheduleView.items=[detail('1').control];const p=s.c.loadScheduleDetail('');
     const d=detail('1');d.control.repeatable=0;s.requests[0].resolve(resp(d));await p;assert.ok(s.nodes['schedule-list'].innerHTML.includes('已停止轮询'));}
    // Zero is a default interval, not a busy loop; anomalous members cannot retry.
    {const s=setup(),d=detail('1');d.control.repeat_interval=0;s.c.scheduleView.items=[d.control];s.c.renderScheduleList(s.c.document.getElementById('schedule-list'));
     assert.ok(s.nodes['schedule-list'].innerHTML.includes('使用站点默认周期'));assert.ok(!s.nodes['schedule-list'].innerHTML.includes('0 秒'));
     d.members[0].repeatable=1;s.c.renderScheduleDetail(s.c.document.getElementById('schedule-detail-body'),d);
     assert.ok(!s.nodes['schedule-detail-body'].innerHTML.includes('原批重试</button>'));assert.ok(s.nodes['schedule-detail-body'].innerHTML.includes('关闭成员轮询'));
     d.batch.RootID=d.members[0].id='9007199254740993';s.c.renderScheduleDetail(s.nodes['schedule-detail-body'],d);assert.ok(s.nodes['schedule-detail-body'].innerHTML.includes('#9007199254740993 · 根任务'));
     d.batch.RootID='0';d.batch.Failed=0;s.c.renderScheduleDetail(s.nodes['schedule-detail-body'],d);assert.ok(s.nodes['schedule-detail-body'].innerHTML.includes('根任务待恢复'));}
})().catch(e=>{console.error(e);process.exitCode=1;});
