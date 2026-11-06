'use strict';
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm'),assert=require('node:assert/strict');
const nodes={};function node(id){return nodes[id]||(nodes[id]={value:'',innerHTML:'',textContent:'',disabled:false,focus:()=>{}});}
let click;
const c=vm.createContext({console,Response,Date,URLSearchParams,JSON,currentModule:'quality',currentSite:null,authGeneration:1,
 document:{getElementById:node,addEventListener:(name,fn)=>{if(name==='click')click=fn;}},
 esc:v=>String(v??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),apiFetch:async()=>null});
vm.runInContext(fs.readFileSync(path.join(process.argv[2],'quality.js'),'utf8'),c);
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const response=d=>new Response(JSON.stringify(d));
const obs=id=>({id,created_at:'2026-11-06T00:00:00Z',sequence:'9007199254740993',scope:{site:'s',stage:'catalog',rule_id:id,version:'v',release_id:'rel',release_generation:'9007199254740993'},raw_rows:2,fields:[]});
function form(){node('quality-mode').value='observations';node('quality-from').value='2026-11-05T00:00';node('quality-until').value='2026-11-06T00:00';}
(async()=>{
 form();const old=deferred(),fresh=deferred();let n=0;c.apiFetch=()=>++n===1?old.promise:fresh.promise;
 const p1=c.queryQuality(true),p2=c.queryQuality(true);fresh.resolve(response({items:[obs('new')],next:'9007199254740992',cutoff:'9007199254740993',scanned:50}));await p2;
 old.resolve(response({items:[obs('old')],next:'0',cutoff:'1',scanned:1}));await p1;assert.ok(node('quality-list').innerHTML.includes('new'));assert.ok(!node('quality-list').innerHTML.includes('old'));
 let url;c.apiFetch=async u=>{url=u;return response({items:[],next:'0',cutoff:'9007199254740993',scanned:3});};await c.queryQuality(false);
 assert.ok(url.includes('before=9007199254740992'));assert.ok(url.includes('cutoff=9007199254740993'));assert.equal(node('quality-next').disabled,true);
 const read=deferred();c.apiFetch=()=>read.promise;const detail=c.inspectQuality('late','',false);const before=node('quality-detail').innerHTML;c.editQuality();read.resolve(response(obs('late')));await detail;assert.equal(node('quality-detail').innerHTML,'');
 form();const session=deferred();c.apiFetch=()=>session.promise;const list=c.queryQuality(true);c.authGeneration++;session.resolve(response({items:[obs('private')],scanned:1}));await list;assert.ok(!node('quality-list').innerHTML.includes('private'));
 form();const body=deferred();c.apiFetch=async()=>({ok:true,json:()=>body.promise});const delayed=c.queryQuality(true);await Promise.resolve();c.currentSite='other';body.resolve({items:[obs('body-late')],scanned:1});await delayed;assert.ok(!node('quality-list').innerHTML.includes('body-late'));
 c.currentSite=null;c.apiFetch=async()=>response(obs('<img onerror=bad>'));await c.inspectQuality('unsafe','',false);assert.ok(!node('quality-detail').innerHTML.includes('<img'));assert.ok(node('quality-detail').innerHTML.includes('&lt;img'));assert.ok(node('quality-detail').innerHTML.includes('9007199254740993'));
 form();c.qualityView.filter=c.qualityFilters();c.apiFetch=async u=>{url=u;return new Response('upstream failed',{status:500});};await c.summarizeQuality();assert.ok(url.includes('release_generation=9007199254740993'));assert.equal(node('quality-detail').textContent,'upstream failed');
 c.apiFetch=async()=>new Response('expired reference',{status:404});await c.inspectQuality('gone','report',false);assert.equal(node('quality-detail').textContent,'expired reference');
 form();node('quality-until').value='2026-12-01T00:00';let calls=0;c.apiFetch=async()=>{calls++;};await c.queryQuality(true);assert.equal(calls,0);assert.ok(node('quality-list').textContent.includes('最多 7 天'));
 console.log('quality behavior passed');
})().catch(e=>{console.error(e);process.exitCode=1;});
