'use strict';
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm'),assert=require('node:assert/strict');
const nodes={},saved=new Map(),toasts=[];
function node(id){return nodes[id]||(nodes[id]={value:'',textContent:'',innerHTML:'',hidden:false,disabled:false,checked:false,querySelectorAll:()=>[],scrollIntoView:()=>{},focus:()=>{}});}
const c=vm.createContext({console,Response,URLSearchParams,JSON,crypto:{randomUUID:()=> 'unique-operation'},currentModule:'rules',currentSite:'shop',authGeneration:1,
 document:{getElementById:node,addEventListener:()=>{}},sessionStorage:{getItem:k=>saved.get(k),setItem:(k,v)=>saved.set(k,v),removeItem:k=>saved.delete(k)},
 Toast:{err:s=>toasts.push(s)},Dialog:{confirm:async()=>true},esc:v=>String(v??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),
 apiFetch:async()=>null,apiRequest:async()=>null,responseError:async r=>r.text()});
vm.runInContext(fs.readFileSync(path.join(process.argv[2],'rules.js'),'utf8'),c);
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const response=data=>new Response(JSON.stringify(data));
function plan(){return {kind:'publish',site:'shop',generation:'9007199254740993',bindings:{list:{id:'catalog',version:'v2'}}};}
function form(){node('rule-site').value='shop';node('rule-generation').textContent='9007199254740993';node('rule-bindings').value=JSON.stringify(plan().bindings);}
function review(){form();const data={plan:plan(),stages:[],verification_ids:{list:'proof'}};c.ruleOps.preview={data,key:c.ruleFormKey(),epoch:c.authGeneration,seq:c.ruleOps.previewSeq};node('rule-reviewed').checked=true;}
(async()=>{
 const uuid=c.crypto.randomUUID;c.crypto.randomUUID=undefined;c.crypto.getRandomValues=bytes=>bytes.fill(123);assert.equal(c.ruleOperationID(),'oa-'+'7b'.repeat(16));c.crypto.randomUUID=uuid;
 // Older requests (including bodies) cannot replace new selections or sessions.
 const older=deferred(),newer=deferred();let call=0;
 c.apiFetch=()=> ++call===1?older.promise:newer.promise;
 const old=c.loadRuleVersions(true),fresh=c.loadRuleVersions(true);
 newer.resolve(response({items:[{id:'new',version:'2',state:'draft'}],next:null}));await fresh;
 older.resolve(response({items:[{id:'old',version:'1'}],next:null}));await old;assert.ok(node('rule-versions').innerHTML.includes('new'));assert.ok(!node('rule-versions').innerHTML.includes('old'));
 form();const releases=deferred();c.apiFetch=()=>releases.promise;const loading=c.loadRuleReleases(true);node('rule-site').value='other';c.editRuleSite();releases.resolve(response({current:{generation:'99'},items:[],next:'0'}));await loading;assert.equal(node('rule-generation').textContent,'—');assert.equal(c.ruleOps.current,null);
 form();const preview=deferred();c.apiRequest=()=>preview.promise;const reviewing=c.previewRuleChange('publish');node('rule-bindings').value='{}';c.invalidateRulePreview();preview.resolve(response({plan:plan(),stages:[],verification_ids:{}}));await reviewing;assert.equal(c.ruleOps.preview,null);
 // Cancel, changed form and changed authentication never dispatch a write.
 let writes=0;c.apiRequest=async()=>{writes++;return null;};review();c.Dialog.confirm=async()=>false;await c.confirmRuleChange();assert.equal(writes,0);
 review();c.Dialog.confirm=async()=>{node('rule-bindings').value='{}';return true;};await c.confirmRuleChange();assert.equal(writes,0);
 review();c.Dialog.confirm=async()=>{c.authGeneration++;return true;};await c.confirmRuleChange();assert.equal(writes,0);
 review();c.Dialog.confirm=async()=>true;const originalSet=c.sessionStorage.setItem;c.sessionStorage.setItem=()=>{throw Error('denied');};await c.confirmRuleChange();assert.equal(writes,0);c.sessionStorage.setItem=originalSet;
 // Unknown outcome persists the exact operation before dispatch; double click
 // while confirmation is open cannot create a second operation.
 review();const confirm=deferred();c.Dialog.confirm=()=>confirm.promise;const confirmation=c.confirmRuleChange();await c.confirmRuleChange();assert.equal(writes,0);confirm.resolve(true);await confirmation;assert.equal(writes,1);assert.ok(saved.has(c.rulePendingKey));const request=JSON.parse(saved.get(c.rulePendingKey));assert.equal(request.generation,'9007199254740993');
 c.ruleOps.pending=null;c.restoreRulePending();assert.equal(c.ruleOps.pending.operation_id,request.operation_id);
 // Corruption after COMMIT and unknown network errors retain the ID. Only a
 // definite rejection marker may clear it. Changed operators can still look up.
 c.apiRequest=async()=>new Response('integrity failure',{status:409});await c.sendRulePending();assert.ok(c.ruleOps.pending);
 c.apiRequest=async()=>new Response('different operator',{status:409});await c.sendRulePending();assert.ok(c.ruleOps.pending);
 c.apiRequest=async()=>new Response('stale preview',{status:409,headers:{'X-Rule-Outcome':'rejected'}});await c.sendRulePending();assert.equal(c.ruleOps.pending,null);assert.ok(!saved.has(c.rulePendingKey));
 // Recovery reads, never replays automatically; manual resend keeps every byte.
 c.ruleOps.pending=request;saved.set(c.rulePendingKey,JSON.stringify(request));c.Dialog.confirm=async()=>true;let dispatched;
 c.apiRequest=async(u,o)=>{dispatched=JSON.parse(o.body);return null;};await c.resolveRulePending(true);assert.deepEqual(dispatched,request);
 c.apiFetch=async()=>response({operation_id:request.operation_id,site:'shop',generation:'9007199254740994',id:'release'});node('rule-site').value='other';await c.resolveRulePending(false);assert.equal(c.ruleOps.pending,null);assert.ok(node('rule-result').innerHTML.includes('9007199254740994'));
 // Report/rule values are always escaped, never active HTML.
 c.apiFetch=async()=>response({version:{Digest:'sha',Rule:{selector:'<img onerror=bad>'}},verification:null});await c.inspectRule('<script>','1');assert.ok(!node('rule-detail').innerHTML.includes('<script>'));assert.ok(node('rule-detail').innerHTML.includes('&lt;img'));
 const inflight=deferred();c.apiFetch=()=>inflight.promise;const inspecting=c.inspectRule('late','1');const before=node('rule-detail').innerHTML;c.authGeneration++;inflight.resolve(response({version:{Digest:'late',Rule:{}},verification:null}));await inspecting;assert.equal(node('rule-detail').innerHTML,before);
 console.log('rule operation behavior passed');
})().catch(e=>{console.error(e);process.exitCode=1;});
