'use strict';
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm'),assert=require('node:assert/strict');
const nodes={};function node(id){return nodes[id]||(nodes[id]={value:'',innerHTML:'',textContent:'',checked:true,disabled:false,focus:()=>{},querySelectorAll:()=>[{value:'evidence'}]});}
const storage=new Map(),calls=[];let confirm=async()=>true;
const c=vm.createContext({console,Response,Date,URLSearchParams,JSON,currentModule:'agents',currentSite:null,authGeneration:1,
 document:{getElementById:node,addEventListener:()=>{}},esc:v=>String(v??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),
 sessionStorage:{getItem:k=>storage.get(k),setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)},Dialog:{confirm:(...args)=>confirm(...args)},Toast:{err:()=>{}},ruleOperationID:()=> 'operation',ruleTime:String,qualityLocalTime:d=>d.toISOString().slice(0,19),
 apiFetch:async()=>null,apiRequest:async(u,o)=>{calls.push([u,JSON.parse(o.body)]);return new Response(JSON.stringify({id:'request',operation_id:'operation',report_id:'report',revision:'1',status:'pending'}));}});
vm.runInContext(fs.readFileSync(path.join(process.argv[2],'agents.js'),'utf8'),c);
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const selection={report_id:'report',profile_id:'approved',evidence_ids:['evidence']};
const preview={selection,profile:{label:'approved'},digest:'sha',plan:{}};
(async()=>{
 c.agentView.caps={profiles:[],quality:true};c.agentView.report={id:'report'};node('agent-report-id').value='report';node('agent-profile').value='approved';c.agentView.preview=preview;
 await c.agentMutate('prepare');assert.equal(calls.length,1);assert.ok(storage.get('papa-agent-operation-v1'));assert.equal(c.agentView.request.id,'request');
 await c.agentMutate('prepare');assert.equal(calls.length,1,'cannot overwrite pending operation');
 c.agentView.preview=preview;node('agent-reviewed').checked=true;await c.agentMutate('start');assert.equal(calls.length,2);assert.equal(calls[1][0],'/api/agents/start');assert.equal(calls[1][1].id,'request');assert.ok(JSON.parse(storage.get('papa-agent-operation-v1')).start);
 c.agentView.anchor=null;c.agentRestore();assert.equal(calls.length,2,'restoring must never POST');
 c.apiFetch=async u=>{assert.ok(u.includes('/operation?'));return new Response(JSON.stringify({id:'request',operation_id:'operation',report_id:'report',revision:'2',status:'running'}));};await c.resolveAgent(false);assert.equal(calls.length,2);c.clearAgentAnchor();assert.ok(c.agentView.anchor,'running anchor cannot be discarded');
 const old=deferred(),fresh=deferred();let n=0;c.apiFetch=()=>++n===1?old.promise:fresh.promise;const a=c.agentOpenRequest('old'),b=c.agentOpenRequest('new');fresh.resolve(new Response(JSON.stringify({id:'new',status:'pending'})));await b;old.resolve(new Response(JSON.stringify({id:'old',status:'pending'})));await a;assert.equal(c.agentView.request.id,'new');
 const late=deferred();c.apiFetch=async()=>({ok:true,json:()=>late.promise});const read=c.agentOpenRequest('late');await Promise.resolve();c.authGeneration++;late.resolve({id:'late',status:'running'});await read;assert.equal(c.agentView.request,null,'auth change must leave no stale request actionable');
 c.agentView.preview=preview;c.agentView.request={id:'request',report_id:'report',operation_id:'operation',revision:'1',status:'pending'};node('agent-reviewed').checked=true;const dialog=deferred();confirm=()=>dialog.promise;const start=c.agentMutate('start');c.invalidateAgentPreview();dialog.resolve(true);await start;assert.equal(calls.length,2,'stale confirmation starts nothing');
 c.agentShowRequest({id:'<img onerror=evil>',status:'pending'});assert.ok(!node('agent-detail').innerHTML.includes('<img'));
 assert.ok(!calls.some(x=>x[0].includes('/rules/change')));
 // A persisted pending request is recoverable without the old tab's storage.
 confirm=async()=>true;c.agentView.anchor=null;storage.clear();c.agentView.preview=preview;
 c.agentView.request={id:'persisted',operation_id:'persisted-operation',report_id:'report',status:'pending',revision:'1'};
 const beforeRecover=calls.length;await c.agentMutate('start');
 assert.equal(calls.length,beforeRecover+1,'pending request cannot start after browser storage loss');
 assert.equal(calls.at(-1)[1].id,'persisted');
 assert.equal(JSON.parse(storage.get('papa-agent-operation-v1')).prepare.operation_id,'persisted-operation');
 // Opening another request must not leave a hidden old request startable.
 c.agentView.preview=preview;const pendingRead=deferred();c.apiFetch=()=>pendingRead.promise;
 const switching=c.agentOpenRequest('other-request');const beforeSwitch=calls.length;
 await c.agentMutate('start');assert.equal(calls.length,beforeSwitch,'started hidden previous request during detail load');
 pendingRead.resolve(new Response(JSON.stringify({id:'other-request',status:'pending'})));await switching;
 // A rejected preparation can be reviewed again without minting another ID.
 c.agentView.request=null;c.agentView.anchor={prepare:{...selection,digest:'old',operation_id:'rejected-prepare'}};
 c.agentView.preview=preview;await c.agentMutate('reprepare');
 assert.equal(calls.at(-1)[0],'/api/agents/prepare');assert.equal(calls.at(-1)[1].operation_id,'rejected-prepare');assert.equal(calls.at(-1)[1].digest,'sha');
 const beforeUnsafeRetry=calls.length;c.agentView.request=null;c.agentView.anchor.start={id:'unknown-start'};c.agentView.preview=preview;
 await c.agentMutate('reprepare');assert.equal(calls.length,beforeUnsafeRetry,'unknown execution must remain query-only');
 // Candidate handoff is read-only, preserves every stage and invalidates old approval.
 c.agentView.review={run_id:'run',candidate:{reference:{id:'rule',version:'v2'}}};
 c.ruleOps={releaseSeq:0,current:{},preview:{approved:true}};c.ruleNode=id=>node('rule-'+id);
 c.invalidateRulePreview=()=>{c.ruleOps.preview=null;};c.renderRulePending=()=>{};
 c.switchModule=name=>{c.currentModule=name;};
 c.apiFetch=async u=>{assert.ok(u.includes('/agents/publication?'));return new Response(JSON.stringify({site:'site',generation:'9007199254740993',bindings:{list:{id:'rule',version:'v2'},detail:{id:'detail',version:'v1'}}}));};
 const beforeHandoff=calls.length;await c.agentHandoff();
 assert.equal(calls.length,beforeHandoff);assert.equal(c.ruleOps.preview,null);assert.equal(node('rule-generation').textContent,'9007199254740993');assert.equal(JSON.parse(node('rule-bindings').value).detail.version,'v1');
 c.currentModule='agents';
 c.apiFetch=async()=>null;await c.loadAgents();assert.equal(c.agentView.request,null);assert.equal(node('agent-detail').innerHTML,'');assert.equal(node('agent-evidence').innerHTML,'');
 c.editAgentFilters();assert.ok(node('agent-list').textContent.includes('筛选已改变'));
 let recent=false;c.agentLists=async reset=>{recent=reset;};await c.recentAgents();assert.equal(recent,true);assert.ok(new Date(node('agent-until').value)-new Date(node('agent-from').value)===86400000);
 // Candidate report rendering uses the shared summary when present and keeps
 // the standalone/older asset fallback as escaped JSON.
 assert.ok(c.agentJSON({Report:{Passed:false,Results:[]}}).includes('rule-json'));
 c.ruleFixtureSummary=()=>'<p>negative-summary</p>';
 assert.ok(c.agentJSON({Report:{Passed:true,Results:[]}}).startsWith('<p>negative-summary</p>'));
 console.log('agent behavior passed');
})().catch(e=>{console.error(e);process.exitCode=1;});
