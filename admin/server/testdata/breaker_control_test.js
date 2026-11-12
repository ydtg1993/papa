const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const source = fs.readFileSync(path.join(process.argv[2], 'mo.js'), 'utf8');
const calls=[]; let confirm=false;
const ctx=vm.createContext({
    Dialog:{confirm:async()=>confirm},
    Toast:{err(){},info(){},ok(){}},
    apiPost:async(url,body)=>{calls.push({url,body});return {ok:true,json:async()=>({resumed:true})};},
    fetchData(){}
});
vm.runInContext(source.slice(source.indexOf('async function resumeCrawling('),source.indexOf('/* ---- 站点控制：')),ctx);
(async()=>{
    await ctx.resumeCrawling('fixture');
    assert.equal(calls[0].url,'/api/breaker/resume');
    await ctx.forceResumeCrawling('fixture');
    assert.equal(calls.length,1);
    confirm=true;
    await ctx.forceResumeCrawling('fixture');
    assert.equal(calls[1].url,'/api/breaker/force-resume');
    assert.equal(calls[1].body.scope,'fixture');
    await ctx.forceResumeCrawling('');
    assert.equal(calls.length,2);
})().catch(err=>{console.error(err);process.exitCode=1;});
