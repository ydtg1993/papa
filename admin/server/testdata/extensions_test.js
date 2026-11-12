const controllers=require('./controllers');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm'),assert=require('node:assert/strict');
function element(){return {children:[],dataset:{},append(...v){this.children.push(...v)},replaceChildren(){this.children=[]}}}
const c=vm.createContext({document:{createElement:element}});
controllers.run(fs.readFileSync(path.join(process.argv[2],'extensions.js'),'utf8'),c);
const p=c.Papa,root=element();let allow=false,destroyed=0;
p.page('test',el=>{el.append('page');return {beforeLeave:()=>allow,destroy:()=>destroyed++}});
assert.equal(p.render('test',root),true);assert.equal(p.leave(false),false);assert.equal(destroyed,0);assert.equal(root.children.length,1);
allow=true;assert.equal(p.leave(false),true);assert.equal(destroyed,1);assert.equal(root.children.length,0);
p.leave(true);assert.equal(destroyed,1);
p.settings('test','<script>untrusted label</script>',()=>({beforeLeave:()=>false,destroy:()=>destroyed++}));
p.renderSettings(root);assert.equal(root.children[0].children[0].textContent,'<script>untrusted label</script>');
assert.equal(p.leave(false),false);assert.equal(p.leave(true),true);assert.equal(destroyed,2);
p.page('throws',()=>({destroy(){throw Error('cleanup')}}));p.render('throws',element());p.render('test',element());p.leave(true);assert.equal(destroyed,3);
p.page('legacy',()=>{});p.render('legacy',element());assert.equal(p.leave(false),true);
console.log('extension lifecycle: guards, cleanup, forced logout, safe labels, legacy renderers passed');

function fresh(){const v=vm.createContext({document:{createElement:element}});controllers.run(fs.readFileSync(path.join(process.argv[2],'extensions.js'),'utf8'),v);return v.Papa}
let failures=0;
function regression(name,fn){try{fn();console.log('PASS '+name)}catch(e){failures++;console.error('FAIL '+name+'\n'+e.stack)}}
regression('registry supports all allowed keys without inherited renderers',()=>{
 const api=fresh(),host=element();assert.equal(api.has('constructor'),false);
 api.page('__proto__',el=>el.append('registered'));assert.equal(api.has('__proto__'),true);api.render('__proto__',host);assert.equal(host.children[0],'registered');api.leave(true);
 api.settings('__proto__','Allowed key',el=>el.append('settings'));api.renderSettings(host);assert.equal(host.children.length,1);api.leave(true);
});
regression('remount disposes only the old page and settings lifetimes',()=>{
 const api=fresh(),host=element(),other=element();let stopped=0;
 api.page('p',el=>{el.append('page');return()=>stopped++});api.render('p',host);api.render('p',other);api.render('p',host);assert.equal(stopped,1);assert.equal(host.children.length,1);assert.equal(other.children.length,1);
 api.leave(true);assert.equal(stopped,3);
 api.settings('a','A',()=>()=>stopped++);api.settings('b','B',()=>()=>stopped++);
 api.renderSettings(host);api.renderSettings(host);assert.equal(stopped,5);assert.equal(host.children.length,2);api.leave(true);assert.equal(stopped,7);
});
regression('cleanup reentry preserves the replacement and cancels older navigation',()=>{
 const api=fresh(),host=element();let oldStopped=0,newStopped=0;
 api.page('new',el=>{el.append('new');return()=>newStopped++});
 api.page('old',el=>{el.append('old');return()=>{oldStopped++;api.render('new',host)}});
 api.render('old',host);assert.equal(api.leave(false),false);assert.deepEqual(host.children,['new']);assert.equal(oldStopped,1);
 assert.equal(api.leave(false),true);assert.equal(newStopped,1);
 api.render('old',host);assert.equal(api.leave(true),true);assert.equal(host.children.length,0);assert.equal(oldStopped,2);api.leave(true);assert.equal(newStopped,1);
});
regression('render and guard reentry do not register or clear stale owners',()=>{
 const api=fresh(),host=element();let stopped=0;
 api.page('new',el=>{el.append('new');return()=>stopped++});
 api.page('nested',el=>{el.append('old');api.render('new',host);return()=>stopped++});
 api.render('nested',host);assert.equal(stopped,1);assert.deepEqual(host.children,['new']);api.leave(true);assert.equal(stopped,2);
 api.page('guard',el=>{el.append('guard');return {beforeLeave(){api.render('new',host);return true},destroy(){stopped++}}});
 api.render('guard',host);assert.equal(api.leave(false),false);assert.deepEqual(host.children,['new']);api.leave(true);assert.equal(stopped,4);
});
if(failures)process.exitCode=1;
