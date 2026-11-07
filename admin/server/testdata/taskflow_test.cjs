const {test}=require('node:test');const assert=require('node:assert/strict');const vm=require('node:vm');const fs=require('node:fs');
function harness(){
 class E{constructor(tag){this.tag=tag;this.children=[];this.value='';this.disabled=false;}append(...xs){this.children.push(...xs);}replaceChildren(...xs){this.children=xs;}setAttribute(){}}
 let mount;let approve=true;const calls=[];const pending=[];const c={document:{createElement:t=>new E(t)},Papa:{page:(k,f)=>{mount=f}},window:{confirm:()=>approve},structuredClone,apiRequest:(url,options)=>{calls.push({url,options});return new Promise((resolve,reject)=>pending.push({resolve,reject}));}};
 vm.runInNewContext(fs.readFileSync(__dirname+'/../taskflow.js','utf8'),c);const root=new E('root');const lifecycle=mount(root);
 return{root:root.children[0],host:root,lifecycle,calls,pending,approve:v=>approve=v};
}
const snap=()=>({id:'run',site:'s',revision:4,policy:{stages:{detail:'items'},lanes:{items:{mode:'depth',parallel:1}}},tasks:[]});
test('initial guidance hides apply and empty query never sends a request',async()=>{
 const h=harness();const [, ,id,load,message,,apply,,empty]=h.root.children;
 assert.equal(h.host.className,undefined);assert.equal(h.root.className,'taskflow-panel');assert.equal(load.className,'btn taskflow-query');assert.equal(apply.hidden,true);assert.notEqual(empty.hidden,true);
 await load.onclick();assert.equal(h.calls.length,0);assert.match(message.textContent,/输入/);
 id.value='run';const read=load.onclick();assert.equal(load.textContent,'正在处理…');h.pending.shift().resolve({ok:true,json:async()=>snap()});await read;
 assert.equal(empty.hidden,true);assert.equal(apply.hidden,false);assert.equal(apply.disabled,true);assert.equal(load.textContent,'刷新运行');
});
test('policy page requires explicit confirmation and preserves CAS conflict edits',async()=>{
 const h=harness();const [, ,id,load,message,editor,apply]=h.root.children;id.value='run';const read=load.onclick();h.pending.shift().resolve({ok:true,json:async()=>snap()});await read;
 const mode=editor.children[0].children[0].children[0];mode.value='breadth';mode.onchange();assert.equal(apply.disabled,false);
 h.approve(false);await apply.onclick();assert.equal(h.calls.length,1);
 h.approve(true);const write=apply.onclick();assert.equal(id.disabled,true);const sent=JSON.parse(h.calls[1].options.body);assert.equal(sent.revision,4);assert.equal(sent.policy.lanes.items.mode,'breadth');h.pending.shift().reject(new Error('conflict'));await write;assert.equal(mode.value,'breadth');assert.match(message.textContent,/conflict/);assert.equal(apply.disabled,false);
});
test('destroyed page ignores delayed query and cannot submit',async()=>{
 const h=harness();const id=h.root.children[2],load=h.root.children[3],editor=h.root.children[5];id.value='run';const read=load.onclick();h.lifecycle.destroy();h.pending.shift().resolve({ok:true,json:async()=>snap()});await read;assert.equal(editor.children.length,0);await h.root.children[6].onclick();assert.equal(h.calls.length,1);
});
test('editing run ID cannot discard dirty policy or apply it to another run',async()=>{
 const h=harness();const [, ,id,load,,editor,apply]=h.root.children;id.value='run';const read=load.onclick();h.pending.shift().resolve({ok:true,json:async()=>snap()});await read;
 const mode=editor.children[0].children[0].children[0];mode.value='breadth';mode.onchange();id.value='different';id.oninput();assert.equal(apply.disabled,true);await apply.onclick();assert.equal(h.calls.length,1);
 h.approve(false);await load.onclick();assert.equal(h.calls.length,1);assert.equal(mode.value,'breadth');id.value='run';id.oninput();assert.equal(apply.disabled,false);
});
