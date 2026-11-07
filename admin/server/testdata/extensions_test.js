const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm'),assert=require('node:assert/strict');
function element(){return {children:[],dataset:{},append(...v){this.children.push(...v)},replaceChildren(){this.children=[]}}}
const c=vm.createContext({document:{createElement:element}});
vm.runInContext(fs.readFileSync(path.join(process.argv[2],'extensions.js'),'utf8'),c);
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
