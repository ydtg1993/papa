const controllers=require('./controllers');
const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const dir=process.argv[2],oao=fs.readFileSync(path.join(dir,'oao.js'),'utf8');
let handler;
const c=vm.createContext({URL,encodeURIComponent,document:{addEventListener:(name,fn)=>{assert.equal(name,'oao:table-loaded');handler=fn;}},esc:s=>String(s).replace(/&/g,'&amp;').replace(/"/g,'&quot;').replace(/</g,'&lt;')});
const a=oao.indexOf('    function cell(col, row)'),b=oao.indexOf('    /* ---------- Select',a);assert.ok(a>0&&b>a);
controllers.run(oao.slice(a,b),c);controllers.run(fs.readFileSync(path.join(dir,'table-links.js'),'utf8'),c);
for(const value of ['https://example.com/p?q=a%2Fb&x=1#f','http://example.com/中文','javascript:alert(1)','data:text/html,evil','//evil.example','/relative','https:\\evil.example','https://example.com/\npath']) {
    const col={name:'url',render:'link',href:'{url}',new_tab:true},html=c.cell(col,{url:value});
    const attrs={href:html.match(/href="([^"]*)"/)[1]};
    const link={textContent:value,setAttribute:(k,v)=>attrs[k]=v,removeAttribute:k=>delete attrs[k]};
    handler({detail:{meta:{columns:[{name:'hidden',hidden:true},col]},el:{querySelectorAll:()=>[{children:[{querySelector:()=>link}]}]}}});
    if(value.startsWith('http')&&!value.includes('\\')&&!value.includes('\n')) {assert.equal(attrs.href,new URL(value).href);assert.equal(attrs.rel,'noopener noreferrer');}
    else assert.equal(attrs.href,undefined,value);
}
// A URL-shaped value substituted into a query parameter must remain encoded.
const col={name:'url',render:'link',href:'/open?url={url}'},value='https://a.test/?x=1&other=2';
const html=c.cell(col,{url:value});assert.ok(html.includes(encodeURIComponent(value)));
handler({detail:{meta:{columns:[col]},el:{querySelectorAll:()=>{throw Error('parameter template touched');}}}});
