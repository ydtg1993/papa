const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
const source=fs.readFileSync(process.argv[2],'utf8');
function section(start,end){const a=source.indexOf(start),b=source.indexOf(end,a);assert.ok(a>=0&&b>a);return source.slice(a,b);}
const code=section('    async function apiFetch(', '    async function apiPost(')+section('    async function downloadFile(', '    function viewLog(')+section('    function invalidateBreakerAnalysis(', '    var breakerAnalysisSeq = 0;');
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve};};
function setup(){
 let key='old',active=true;const requests=[],effects=[];
 const c=vm.createContext({currentModule:'dashboard',currentSite:'a',breakerAnalysisSeq:1,
  loadKey:()=>key,clearKey:()=>effects.push('clear'),showLogin:()=>effects.push('login'),setStatus:()=>effects.push('status'),alert:()=>effects.push('alert'),
  fetch:()=>{const d=deferred();requests.push(d);return d.promise;},
  URL:{createObjectURL:()=>{effects.push('blob');return 'blob:test';},revokeObjectURL:()=>{}},
  document:{getElementById:()=>({}),createElement:()=>({click:()=>effects.push('download'),remove:()=>{}}),body:{appendChild:()=>{}}},
  Dialog:{close:()=>effects.push('close')}
 });vm.runInContext(code,c);
 return {c,requests,effects,current:()=>active,retire:()=>{active=false;key='new';}};
}
(async()=>{
 const stale=setup();let p=stale.c.apiFetch('/analysis',{current:stale.current});stale.retire();stale.requests[0].resolve({status:401,ok:false});assert.equal(await p,null);assert.deepEqual(stale.effects,[]);
 const failed=setup();p=failed.c.apiFetch('/analysis',{current:failed.current});const error={status:404,ok:false};failed.requests[0].resolve(error);assert.equal(await p,error,'diagnostic error details must reach dialog');
 const legacy=setup();p=legacy.c.apiFetch('/legacy');legacy.requests[0].resolve(error);assert.equal(await p,null,'legacy request behavior changed');
 const download=setup(),blob=deferred();p=download.c.downloadFile('/analysis?format=text',download.current);download.requests[0].resolve({status:200,ok:true,blob:()=>blob.promise,headers:{get:()=>''}});await Promise.resolve();download.retire();blob.resolve({});await p;assert.deepEqual(download.effects,[],'stale blob downloaded');
 const scoped=setup();const original=scoped.c.captureBreakerAnalysisView();assert.ok(original());scoped.c.invalidateBreakerAnalysis();assert.ok(!original(),'A to B to A must invalidate original view');assert.deepEqual(scoped.effects,['close']);
})().catch(e=>{console.error(e);process.exitCode=1;});
