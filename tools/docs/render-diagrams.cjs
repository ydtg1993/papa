// Source-owned documentation graphics. Run from any directory; no network at render time.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const {chromium} = require('playwright');
const root = path.resolve(__dirname, '../..');
const folder = path.join(root, 'docs/assets/diagrams');
const titles = {
  '01-capabilities': ['能力总览', '框架负责运行机制，业务负责站点协议、结果模型与幂等性。'],
  '02-components': ['组件关系', '箭头表示运行协作，不是 Go import 许可；公共门面不复制领域状态。'],
  '03-task-loop': ['动态数据流', 'Task 是调度单位；一个 Task 可以发出多个请求，业务产出由 Fetcher 显式处理。'],
  '04-recovery-budget': ['积压、背压与恢复', '驻留令牌限制内存工作集；崩溃恢复可能重跑业务，业务副作用必须幂等。'],
  '05-observation': ['可靠控制与观测证据', '在线报告只说明收到的信号；缺失证据不能证明任务未执行或执行成功。'],
  '06-lifecycle': ['取消、排空与依赖关闭', '不是全 App 共用一个 2 秒期限；取消信号也不等于在途工作已经退出。']
};
const hash = data => crypto.createHash('sha256').update(data).digest('hex');
const esc = s => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');

(async () => {
  const browser = await chromium.launch({headless:true, ...(process.env.PAPA_DOCS_BROWSER ? {executablePath:process.env.PAPA_DOCS_BROWSER} : {})});
  const manifest = {mermaid:require('mermaid/package.json').version, playwright:require('playwright/package.json').version, diagrams:[]};
  try {
    const page = await browser.newPage({viewport:{width:1600,height:1200},deviceScaleFactor:1});
    await page.setContent('<html><body style="margin:0;background:white"></body></html>');
    await page.addScriptTag({path:require.resolve('mermaid/dist/mermaid.min.js')});
    for (const [name, [title, note]] of Object.entries(titles)) {
      const source = fs.readFileSync(path.join(folder, name+'.mmd'));
      const result = await page.evaluate(async ({code,name}) => {
        mermaid.initialize({startOnLoad:false,securityLevel:'strict',theme:'base',deterministicIds:true,deterministicIDSeed:name,markdownAutoWrap:false,
          fontFamily:'Microsoft YaHei, Noto Sans CJK SC, Arial, sans-serif',
          themeVariables:{fontSize:'18px',primaryColor:'#e7f5ef',primaryTextColor:'#173b58',primaryBorderColor:'#449c7a',lineColor:'#5b7584',clusterBkg:'#f6f9fb',clusterBorder:'#d2dfe6',edgeLabelBackground:'#ffffff'},
          flowchart:{htmlLabels:false,useMaxWidth:false,curve:'linear',nodeSpacing:30,rankSpacing:45,padding:16,wrappingWidth:320}});
        await mermaid.parse(code);
        const {svg} = await mermaid.render('papa-'+name,code);
        document.body.innerHTML = svg;
        const box = document.querySelector('svg').viewBox.baseVal;
        return {svg,w:box.width,h:box.height};
      }, {code:source.toString('utf8'),name});
      const width = Math.max(1200,Math.ceil(result.w+80)), height = Math.ceil(result.h+210);
      const inner = result.svg.replace('<svg ', `<svg x="${(width-result.w)/2}" y="128" `);
      const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" role="img" aria-label="${esc(title)}"><title>Papa v4 · ${esc(title)}</title><desc>${esc(note)}</desc><rect width="100%" height="100%" fill="#fff"/><rect width="12" height="90" x="32" y="20" rx="6" fill="#257d6b"/><g font-family="Microsoft YaHei, Noto Sans CJK SC, Arial, sans-serif"><text x="62" y="48" font-size="17" fill="#517589" letter-spacing="3">PAPA v4 / SINGLE INSTANCE</text><text x="60" y="91" font-size="34" font-weight="700" fill="#123249">${esc(title)}</text></g>${inner}<line x1="32" y1="${height-53}" x2="${width-32}" y2="${height-53}" stroke="#d7e2e7"/><text x="40" y="${height-22}" font-family="Microsoft YaHei, Noto Sans CJK SC, Arial, sans-serif" font-size="17" fill="#456171">${esc(note)}</text></svg>`;
      const svgPath = path.join(folder,name+'.svg'), pngPath = path.join(folder,name+'.png');
      fs.writeFileSync(svgPath,svg);
      await page.evaluate(svg => {document.body.innerHTML=svg;},svg);
      await page.locator('body > svg').screenshot({path:pngPath});
      manifest.diagrams.push({name,title,width,height,sourceSHA256:hash(source),svgSHA256:hash(fs.readFileSync(svgPath)),pngSHA256:hash(fs.readFileSync(pngPath))});
      console.log(`${name}: ${width} x ${height}`);
    }
    fs.writeFileSync(path.join(folder,'manifest.json'),JSON.stringify(manifest,null,2)+'\n');
  } finally {await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
