'use strict';
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm'),assert=require('node:assert/strict');
const nodes={},storage=new Map(),writes=[];let storageFails=false;
const node=id=>nodes[id]||(nodes[id]={innerHTML:'',textContent:'',focus:()=>{}});
const report={query:{scope:{site:'s',release_generation:'9007199254740993'},limit:1000},input_sha256:'sha'};
const c=vm.createContext({console,Response,JSON,currentModule:'quality',authGeneration:1,qualityView:{detailSeq:1,report},qualityNode:node,esc:String,Toast:{err:()=>{}},Dialog:{confirm:async()=>true},ruleOperationID:()=> 'freeze-op',qualityReport:r=>r.id,
sessionStorage:{getItem:k=>storage.get(k),setItem:(k,v)=>{if(storageFails)throw Error('storage denied');storage.set(k,v);},removeItem:k=>storage.delete(k)},
apiRequest:async(u,o)=>{writes.push([u,JSON.parse(o.body)]);return new Response('unknown outcome',{status:500});},apiFetch:async()=>new Response(JSON.stringify({freeze:true}))});
vm.runInContext(fs.readFileSync(path.join(process.argv[2],'quality-write.js'),'utf8'),c);
(async()=>{await c.loadQualityWrites();storageFails=true;await c.freezeQuality();assert.equal(writes.length,0,'write without durable browser anchor');storageFails=false;await c.freezeQuality();assert.equal(writes.length,1);assert.ok(storage.get('papa-quality-freeze-v1'));assert.equal(writes[0][1].query.scope.release_generation,'9007199254740993');await c.freezeQuality();assert.equal(writes.length,1,'unknown write overwritten');
 c.qualityWrite.pending=null;await c.loadQualityWrites();assert.equal(writes.length,1,'restore posted automatically');assert.equal(c.qualityWrite.pending.operation_id,'freeze-op');
 c.qualityWrite.changed=true;
 c.qualityRead=async()=>({id:'frozen',operation_id:'freeze-op',input_sha256:'sha'});await c.resolveQualityFreeze(false);assert.equal(c.qualityView.report.id,'frozen');assert.equal(storage.size,0);
 assert.equal(c.qualityWrite.changed,false,'prior review-changed flag survived successful resolution');
 c.qualityView.report=report;c.apiRequest=async()=>new Response('review changed',{status:409,headers:{'X-Quality-Outcome':'review-changed'}});await c.freezeQuality();assert.equal(c.qualityWrite.changed,true);c.apiFetch=async()=>new Response('not found',{status:404});await c.withdrawQualityFreeze();assert.equal(storage.size,0);assert.equal(c.qualityWrite.pending,null);
 console.log('quality write behavior passed');})().catch(e=>{console.error(e);process.exitCode=1;});
