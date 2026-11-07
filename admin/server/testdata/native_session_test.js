'use strict';
const {test}=require('node:test');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm'),assert=require('node:assert/strict');
const sourceDir=process.argv[2];
const deferred=()=>{let resolve,reject;const promise=new Promise((r,j)=>{resolve=r;reject=j;});return {promise,resolve,reject};};
function harness(module){
 const nodes=new Map(),storage=new Map(),posts=[],toasts=[];
 const node=id=>{if(!nodes.has(id))nodes.set(id,{value:'',textContent:'',innerHTML:'',hidden:false,disabled:false,checked:false,querySelectorAll:()=>[],focus(){},scrollIntoView(){}});return nodes.get(id);};
 const c=vm.createContext({console,Response,URLSearchParams,Date,JSON,currentModule:module,currentSite:null,authGeneration:1,crypto:{randomUUID:()=> 'synthetic-operation'},
  document:{getElementById:node,addEventListener(){},body:{},activeElement:null},esc:v=>String(v??''),
  sessionStorage:{getItem:k=>storage.get(k),setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)},
  Dialog:{confirm:async()=>true},Toast:{err:m=>toasts.push(m),ok:m=>toasts.push(m)},
  apiFetch:async()=>null,apiRequest:async(u,o)=>{posts.push([u,o]);return null;},apiPost:async(u,o)=>{posts.push([u,o]);return null;}});
 for(const file of ['schedules.js','rules.js','quality.js','quality-write.js','agents.js'])vm.runInContext(fs.readFileSync(path.join(sourceDir,file),'utf8'),c);
 const reset=name=>{c.authGeneration++;if(typeof c[name]==='function')c[name]();};
 return {c,node,nodes,storage,posts,toasts,reset};
}
function mark(h,ids){ids.forEach(id=>{h.node(id).innerHTML='operator A evidence';h.node(id).value='operator A draft';});}
function empty(h,ids){ids.forEach(id=>assert.equal(h.node(id).innerHTML,'',id+' retained the previous operator’s evidence'));}

test('schedule identity reset removes rendered batches and actionable snapshots',()=>{
 const h=harness('schedules'),c=h.c;
 const ids=['schedule-list','schedule-detail-body'];mark(h,ids);
 c.scheduleView.data={control:{id:'old'}};c.scheduleView.items=[{id:'old'}];c.scheduleView.id='old';c.scheduleView.history=['old'];
 h.reset('resetSchedulesSession');empty(h,ids);
 assert.equal(c.scheduleView.data,null);assert.equal(c.scheduleView.items.length,0);assert.equal(c.scheduleView.id,'');
 assert.equal(h.node('schedule-detail').hidden,true);assert.equal(h.node('schedule-list-next').disabled,true);
 assert.equal(h.posts.length,0);
});
test('rule identity reset clears publication forms while retaining durable operation recovery',()=>{
 const h=harness('rules'),c=h.c,ids=['rule-versions','rule-detail','rule-releases','rule-preview','rule-result','rule-pending'];mark(h,ids);
 ['rule-site','rule-filter','rule-bindings'].forEach(id=>h.node(id).value='operator A draft');
 h.storage.set(c.rulePendingKey,'durable original operation');c.ruleOps.pending={operation_id:'old'};c.ruleOps.current={id:'old'};c.ruleOps.preview={approved:true};
 h.reset('resetRulesSession');empty(h,ids);
 assert.equal(c.ruleOps.pending,null);assert.equal(c.ruleOps.current,null);assert.equal(c.ruleOps.preview,null);
 ['rule-site','rule-filter','rule-bindings'].forEach(id=>assert.equal(h.node(id).value,''));
 assert.equal(h.node('rule-generation').textContent,'—');assert.equal(h.node('rule-publish-preview').disabled,true);
 assert.equal(h.storage.get(c.rulePendingKey),'durable original operation');assert.equal(h.posts.length,0);
});
test('quality identity reset clears summaries, write permissions and in-memory recovery only',()=>{
 const h=harness('quality'),c=h.c,ids=['quality-list','quality-detail','quality-pending'];mark(h,ids);
 c.qualityView.report={id:'old'};c.qualityView.observation={id:'old'};c.qualityView.items=[{id:'old'}];c.qualityWrite.enabled=true;c.qualityWrite.pending={operation_id:'old'};
 h.storage.set(c.qualityPendingKey,'durable original freeze');h.reset('resetQualitySession');empty(h,ids);
 assert.equal(c.qualityView.report,null);assert.equal(c.qualityView.observation,null);assert.equal(c.qualityView.items.length,0);
 assert.equal(c.qualityWrite.enabled,false);assert.equal(c.qualityWrite.pending,null);assert.equal(h.node('quality-next').disabled,true);
 assert.equal(h.storage.get(c.qualityPendingKey),'durable original freeze');assert.equal(h.posts.length,0);
});
test('agent identity reset removes reports, profiles and approval without replaying recovery',()=>{
 const h=harness('agents'),c=h.c,ids=['agent-list','agent-detail','agent-evidence','agent-preview','agent-profile','agent-notice','agent-pending'];mark(h,ids);
 c.agentView.caps={profiles:[{id:'old'}]};c.agentView.anchor={prepare:{operation_id:'old'}};c.agentView.preview={approved:true};c.agentView.request={id:'old'};c.agentView.review={id:'old'};c.agentView.runs=[{id:'old'}];
 h.node('agent-report-id').value='old';h.storage.set(c.agentPendingKey,'durable original analysis');h.reset('resetAgentsSession');empty(h,ids);
 for(const key of ['caps','anchor','preview','request','review','report','run'])assert.equal(c.agentView[key],null,key);
 assert.equal(c.agentView.runs.length,0);assert.equal(h.node('agent-report-id').value,'');
 assert.equal(h.storage.get(c.agentPendingKey),'durable original analysis');assert.equal(h.posts.length,0);
});
test('old rule confirmation cannot unlock a new identity or publish',async()=>{
 const h=harness('rules'),c=h.c,dialog=deferred();
 h.node('rule-site').value='s';h.node('rule-generation').textContent='1';h.node('rule-bindings').value='{}';h.node('rule-reviewed').checked=true;
 c.ruleOps.preview={data:{plan:{kind:'publish',site:'s',generation:'1'}},key:c.ruleFormKey(),epoch:1};c.Dialog.confirm=()=>dialog.promise;
 const pending=c.confirmRuleChange();h.reset('resetRulesSession');c.ruleOps.busy=true;dialog.resolve(true);await pending;
 assert.equal(c.ruleOps.busy,true,'old rule confirmation unlocked the new identity');assert.equal(h.posts.length,0);
});
test('old quality confirmation cannot unlock a new identity or freeze',async()=>{
 const h=harness('quality'),c=h.c,dialog=deferred();c.qualityWrite.enabled=true;c.qualityView.report={query:{},input_sha256:'old'};c.Dialog.confirm=()=>dialog.promise;
 const pending=c.freezeQuality();h.reset('resetQualitySession');c.qualityWrite.busy=true;dialog.resolve(true);await pending;
 assert.equal(c.qualityWrite.busy,true,'old quality confirmation unlocked the new identity');assert.equal(h.posts.length,0);
});
test('old agent confirmation cannot unlock a new identity or mutate a request',async()=>{
 const h=harness('agents'),c=h.c,dialog=deferred();c.agentView.request={id:'old',revision:'1'};c.Dialog.confirm=()=>dialog.promise;
 const pending=c.agentMutate('dismiss');h.reset('resetAgentsSession');c.agentView.busy=true;dialog.resolve(true);await pending;
 assert.equal(c.agentView.busy,true,'old agent confirmation unlocked the new identity');assert.equal(h.posts.length,0);
});
test('old quality capability failure cannot repopulate or lock the new identity',async()=>{
 const h=harness('quality'),c=h.c,request=deferred();c.apiFetch=()=>request.promise;
 const pending=c.loadQualityWrites();h.reset('resetQualitySession');request.reject(Error('old transport failure'));await pending;
 assert.equal(c.qualityWrite.locked,false,'old capability failure locked the new identity');empty(h,['quality-pending']);
});
test('old schedule action response cannot refresh or unlock the new identity',async()=>{
 const h=harness('schedules'),c=h.c,request=deferred();
 c.scheduleView.id='control';c.scheduleView.data={control:{id:'control',status:3},batch:{},rules:{},members:[]};
 c.apiPost=()=>request.promise;
 const pending=c.scheduleTaskAction('trace','control');h.reset('resetSchedulesSession');c.scheduleView.busy=true;
 request.resolve(new Response('{}'));await pending;
 assert.equal(c.scheduleView.busy,true,'old schedule action unlocked the new identity');empty(h,['schedule-detail-body']);assert.equal(h.toasts.length,0);
});
test('old agent start response preserves the new preview and the original recovery record',async()=>{
 const h=harness('agents'),c=h.c,request=deferred();
 const selection={report_id:'report',profile_id:'profile',evidence_ids:['evidence']};
 c.agentView.preview={selection,digest:'sha',profile:{label:'profile'}};c.agentView.request={id:'request',report_id:'report',operation_id:'operation',revision:'1',status:'pending'};
 h.node('agent-reviewed').checked=true;c.apiRequest=(u,o)=>{h.posts.push([u,o]);return request.promise;};
 const pending=c.agentMutate('start');await new Promise(r=>setImmediate(r));
 assert.equal(h.posts.length,1);const original=h.storage.get(c.agentPendingKey);
 h.reset('resetAgentsSession');const preview={newIdentity:true};c.agentView.preview=preview;c.agentView.busy=true;
 request.resolve(new Response(JSON.stringify({id:'request',status:'running'})));await pending;
 assert.equal(c.agentView.preview,preview,'old start response erased the new preview');assert.equal(c.agentView.busy,true);
 assert.equal(h.storage.get(c.agentPendingKey),original,'identity reset erased an unconfirmed model operation');empty(h,['agent-detail']);
});
test('old quality freeze response retains its recovery record after identity reset',async()=>{
 const h=harness('quality'),c=h.c,request=deferred();c.qualityWrite.enabled=true;c.qualityView.report={query:{},input_sha256:'sha'};
 c.apiRequest=(u,o)=>{h.posts.push([u,o]);return request.promise;};
 const pending=c.freezeQuality();await new Promise(r=>setImmediate(r));
 const original=h.storage.get(c.qualityPendingKey);assert.ok(original);h.reset('resetQualitySession');c.qualityWrite.busy=true;
 request.resolve(new Response(JSON.stringify({id:'report',operation_id:'oa-synthetic-operation',input_sha256:'sha'})));await pending;
 assert.equal(c.qualityWrite.busy,true);assert.equal(h.storage.get(c.qualityPendingKey),original);empty(h,['quality-detail','quality-pending']);
});
for(const kind of ['rules','quality','agents'])test('old '+kind+' recovery write cannot settle or unlock the new identity',async()=>{
 const h=harness(kind),c=h.c,request=deferred();
 const pending={operation_id:'original-operation',kind:'publish',site:'s',generation:'1',query:{},input_sha256:'sha',report_id:'report',profile_id:'profile'};
 let state,key,reset,begin;
 if(kind==='rules'){state=c.ruleOps;key=c.rulePendingKey;reset='resetRulesSession';state.pending=pending;begin=()=>c.resolveRulePending(true);}
 else if(kind==='quality'){state=c.qualityWrite;key=c.qualityPendingKey;reset='resetQualitySession';state.pending=pending;begin=()=>c.resolveQualityFreeze(true);}
 else{state=c.agentView;key=c.agentPendingKey;reset='resetAgentsSession';state.anchor={prepare:pending};begin=()=>c.resolveAgent(true);}
 const original=JSON.stringify(kind==='agents'?{prepare:pending}:pending);h.storage.set(key,original);
 c.apiRequest=(u,o)=>{h.posts.push([u,o]);return request.promise;};
 const old=begin();await new Promise(r=>setImmediate(r));assert.equal(h.posts.length,1);
 h.reset(reset);state.busy=true;
 request.resolve(new Response(JSON.stringify({...pending,id:'committed-result',status:'pending'})));await old;
 assert.equal(state.busy,true,'old recovery response unlocked the new identity');assert.equal(h.storage.get(key),original);
 assert.equal(h.posts.length,1,'identity reset automatically replayed a write');assert.equal(h.toasts.length,0);
});
