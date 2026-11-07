'use strict';
const {test}=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
const dir=process.argv[2],read=name=>fs.readFileSync(path.join(dir,name),'utf8');
const template=read('template.html'),head=template.match(/<script>\s*([\s\S]*?)<\/script>/)[1];
const theme=read('mo.js').split('    /* ============ 基础状态 ============ */')[0];
function harness(saved={},failure=''){
    const values=new Map(Object.entries(saved)),writes=[],nodes=new Map(),events=new Map();
    function element(tag='div'){
        const attrs=new Map(),handlers=new Map();
        return {tagName:tag.toUpperCase(),children:[],value:'',textContent:'',disabled:false,dataset:{},classList:{add(name){attrs.set('class',name);}},
            set id(value){nodes.set(value,this);},getAttribute:key=>attrs.get(key)||null,setAttribute:(key,value)=>attrs.set(key,String(value)),
            append(...items){this.children.push(...items);},replaceChildren(){this.children=[];},
            addEventListener(name,fn){if(!handlers.has(name))handlers.set(name,new Set());handlers.get(name).add(fn);},
            removeEventListener(name,fn){handlers.get(name)?.delete(fn);},
            fire(name){handlers.get(name)?.forEach(fn=>fn());}};
    }
    const root=element('html');root.setAttribute('data-auth-required','1');
    const button=element('button');button.id='theme-toggle';
    const document={documentElement:root,getElementById:id=>nodes.get(id),createElement:element,createTextNode:text=>({textContent:text})};
    const storage={getItem(key){if(failure==='read')throw Error('blocked read');return values.get(key)||null;},setItem(key,value){if(failure==='write')throw Error('quota');values.set(key,value);writes.push([key,value]);}};
    const sandbox={document,window:{addEventListener(name,fn){if(!events.has(name))events.set(name,[]);events.get(name).push(fn);}},console,
        fetch(){throw Error('display change must not request data');},switchModule(){throw Error('display change must not navigate');},
        Oao:{motion:{get:()=> 'system',set:value=>{sandbox.motionValue=value;}}}};
    Object.defineProperty(sandbox,'localStorage',{get(){if(failure==='getter')throw Error('blocked storage');return storage;}});
    const context=vm.createContext(sandbox);
    vm.runInContext(head,context);const firstFrame={appearance:root.getAttribute('data-appearance'),theme:root.getAttribute('data-theme'),login:root.getAttribute('class')};
    vm.runInContext(read('extensions.js'),context);vm.runInContext(read('appearance.js'),context);vm.runInContext(theme,context);
    return {c:context,root,button,values,writes,nodes,firstFrame,element,storage,
        fail(value){failure=value;},emit(name,props={}){for(const fn of events.get(name)||[])fn(props);},
        change(id,value){const el=nodes.get(id);el.value=value;el.fire('change');},mount(){const host=element();context.Papa.renderSettings(host);return host;}};
}
test('first paint restores valid independent preferences before stylesheets',()=>{
    assert.ok(template.indexOf('papa-appearance')<template.indexOf('<link rel="stylesheet"'));
    for(const [saved,expected] of [ [{},['simple','light']], [{'papa-appearance':'classic','papa-theme':'dark'},['classic','dark']], [{'papa-appearance':'invalid','papa-theme':'wrong'},['simple','light']] ]){
        const h=harness(saved);assert.deepEqual([h.firstFrame.appearance,h.firstFrame.theme],expected);assert.equal(h.firstFrame.login,'need-login');assert.equal(h.writes.length,0);
    }
    assert.equal(harness({'papa_monitor_key':'synthetic'}).firstFrame.login,null);
});
test('display controls preserve sibling drafts, mount ownership and component state',()=>{
    const h=harness();let renders=0,destroys=0;const input=h.element('input');input.value='unsaved model name';
    h.c.Papa.settings('business','Business',el=>{renders++;el.append(input);return {beforeLeave:()=>false,destroy:()=>destroys++};});
    const host=h.mount(),children=host.children.slice();
    h.change('appearance-style','classic');h.change('appearance-theme','dark');
    assert.equal(h.c.MonitorAppearance.get(),'classic');assert.equal(h.c.getTheme(),'dark');assert.equal(input.value,'unsaved model name');
    assert.deepEqual(host.children,children);assert.equal(renders,1);assert.equal(destroys,0);assert.equal(h.c.Papa.leave(false),false);
    assert.deepEqual(h.writes,[['papa-appearance','classic'],['papa-theme','dark']]);
    h.button.fire('click');assert.equal(h.nodes.get('appearance-theme').value,'light');assert.equal(h.c.MonitorAppearance.get(),'classic');
    h.change('appearance-motion','reduced');assert.equal(h.c.motionValue,'reduced');assert.equal(renders,1);
});
test('cross-tab and restored pages use current stored preferences without writing back',()=>{
    const h=harness();h.mount();
    h.values.set('papa-appearance','classic');h.values.set('papa-theme','dark');
    h.emit('storage',{key:'papa-appearance',storageArea:{}});assert.equal(h.c.MonitorAppearance.get(),'simple');
    h.emit('storage',{key:'papa-appearance',newValue:'old-event',storageArea:h.storage});
    assert.equal(h.nodes.get('appearance-style').value,'classic');assert.equal(h.c.getTheme(),'light');
    h.emit('pageshow');assert.equal(h.nodes.get('appearance-theme').value,'dark');assert.equal(h.writes.length,0);
    h.values.clear();h.emit('storage',{key:null,storageArea:h.storage});
    assert.equal(h.c.MonitorAppearance.get(),'simple');assert.equal(h.c.getTheme(),'light');assert.equal(h.writes.length,0);
});
test('blocked storage preserves current-page switching including repeated dark toggle',()=>{
    for(const mode of ['getter','read','write']){
        const h=harness({'papa-theme':'light'},mode);h.mount();
        h.change('appearance-style','classic');assert.equal(h.c.MonitorAppearance.get(),'classic');
        h.button.fire('click');assert.equal(h.c.getTheme(),'dark');h.button.fire('click');assert.equal(h.c.getTheme(),'light');
        if(mode!=='write')assert.deepEqual([h.firstFrame.appearance,h.firstFrame.theme],['simple','light']);
    }
});
test('destroy unregisters settings controls while preferences remain available',()=>{
    const h=harness();h.mount();const previous=h.nodes.get('appearance-style');
    h.c.Papa.leave(true);previous.value='sentinel';h.c.MonitorAppearance.set('classic');
    assert.equal(previous.value,'sentinel');previous.fire('change');assert.equal(h.c.MonitorAppearance.get(),'classic');
    h.mount();assert.equal(h.nodes.get('appearance-style').value,'classic');
});
test('failed writes survive document restoration while external choices still win',()=>{
    const h=harness({'papa-appearance':'simple','papa-theme':'light'},'write');h.mount();
    h.change('appearance-style','classic');h.change('appearance-theme','dark');h.emit('pageshow');
    assert.equal(h.c.MonitorAppearance.get(),'classic');assert.equal(h.c.getTheme(),'dark');
    h.emit('storage',{key:null,storageArea:h.storage});
    assert.equal(h.c.MonitorAppearance.get(),'simple');assert.equal(h.c.getTheme(),'light');
    h.fail('');h.change('appearance-style','classic');h.change('appearance-theme','dark');h.emit('pageshow');
    assert.equal(h.values.get('papa-appearance'),'classic');assert.equal(h.values.get('papa-theme'),'dark');
});
test('old oao without motion keeps appearance and light-dark controls available',()=>{
    const h=harness();h.c.Oao={};h.mount();assert.equal(h.nodes.get('appearance-motion').disabled,true);
    assert.equal(h.nodes.get('appearance-style').disabled,false);h.change('appearance-style','classic');h.change('appearance-theme','dark');
    assert.equal(h.c.MonitorAppearance.get(),'classic');assert.equal(h.c.getTheme(),'dark');
});
