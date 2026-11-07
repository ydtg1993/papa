const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const mo=fs.readFileSync(path.join(process.argv[2],'mo.js'),'utf8');
const events={};let mask;
const doc={activeElement:null,addEventListener:(key,fn)=>events[key]=fn};
function element(){
    const classes=new Set();let html='';
    const el={children:[],isConnected:true,disabled:false,offsetParent:{},
        classList:{add:s=>classes.add(s),remove:s=>classes.delete(s),contains:s=>classes.has(s),toggle:(s,v)=>v?classes.add(s):classes.delete(s)},
        addEventListener:()=>{},appendChild:b=>el.children.push(b),focus:()=>doc.activeElement=el,blur:()=>doc.activeElement=doc.body,
        contains:b=>el.children.includes(b),querySelector:()=>null,querySelectorAll:()=>el.children.filter(b=>!b.disabled)};
    Object.defineProperty(el,'innerHTML',{get:()=>html,set:v=>{html=v;el.children=[];}});return el;
}
doc.body=element();doc.activeElement=element();const original=doc.activeElement;
const dialog=element(),title=element(),body=element(),actions=element();
actions.querySelector=()=>actions.children[0];
doc.createElement=tag=>{const el=element();if(tag==='div'&&!mask){mask=el;el.querySelector=s=>({'h3':title,'.dlg':dialog,'.dlg-body':body,'.dlg-actions':actions})[s];el.querySelectorAll=()=>actions.children.filter(b=>!b.disabled);el.contains=b=>actions.children.includes(b);}return el;};
const c=vm.createContext({document:doc,Object,Array});
const a=mo.indexOf('    var Dialog ='),b=mo.indexOf('    /* ============',a+10);assert.ok(a>0&&b>a);vm.runInContext(mo.slice(a,b),c);
(async()=>{
    let cancelled=false;
    const old=c.Dialog.confirm({title:'batch operation'}).then(v=>{cancelled=v===false;});
    const newer=c.Dialog.alert({title:'trace response'});await new Promise(resolve=>setImmediate(resolve));
    assert.equal(cancelled,true,'replacing a dialog left the old confirmation pending');
    const button=actions.children[0];button.focus();let prevented=false;
    events.keydown({key:'Tab',preventDefault:()=>prevented=true});assert.equal(prevented,true);assert.equal(doc.activeElement,button);
    button.onclick();await Promise.all([old,newer]);assert.equal(doc.activeElement,original,'dialog did not restore focus');
    const p=c.Dialog.confirm({title:'keyboard'});actions.children[1].focus();prevented=false;
    events.keydown({key:'Tab',preventDefault:()=>prevented=true});assert.equal(prevented,true);assert.equal(doc.activeElement,actions.children[0]);
    events.keydown({key:'Escape',preventDefault:()=>{}});assert.equal(await p,false);
    const detached=element();doc.activeElement=detached;const replaced=c.Dialog.confirm({title:'re-rendered opener'});detached.isConnected=false;
    events.keydown({key:'Escape',preventDefault:()=>{}});await replaced;assert.equal(doc.activeElement,doc.body,'hidden dialog kept focus after opener was removed');
    const secret=c.Dialog.confirm({title:'private identity',body:'plaintext-token'});
    assert.equal(body.innerHTML,'plaintext-token');
    c.Dialog.reset();assert.equal(await secret,false,'session reset left confirmation pending');
    assert.equal(body.innerHTML,'','session reset retained hidden plaintext');assert.equal(actions.children.length,0);assert.equal(title.textContent,'');
})().catch(e=>{console.error(e);process.exitCode=1;});
