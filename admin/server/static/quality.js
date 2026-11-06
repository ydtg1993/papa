'use strict';
var qualityView = {seq:0, detailSeq:0, items:[], next:'', cutoff:'', filter:null, report:null, observation:null};
function qualityNode(id) { return document.getElementById('quality-'+id); }
function invalidateQuality() { qualityView.seq++; qualityView.detailSeq++; }
function editQuality() {
    invalidateQuality(); qualityView.items=[]; qualityView.report=null; qualityView.observation=null; qualityView.filter=null;
    qualityNode('list').textContent='筛选已更改，请查询。'; qualityNode('detail').innerHTML=''; qualityNode('next').disabled=true;
}
function qualityLocalTime(d) { return new Date(d.getTime()-d.getTimezoneOffset()*60000).toISOString().slice(0,19); }
function recentQuality() {
    var now=new Date();qualityNode('until').value=qualityLocalTime(now);qualityNode('from').value=qualityLocalTime(new Date(now.getTime()-86400000));
    return queryQuality(true);
}
function loadQuality() {
    if (!qualityNode('from').value) {var now=new Date();qualityNode('until').value=qualityLocalTime(now);qualityNode('from').value=qualityLocalTime(new Date(now.getTime()-86400000));}
    return queryQuality(true);
}
function qualityFilters() {
    var from=new Date(qualityNode('from').value),until=new Date(qualityNode('until').value);
    if (!Number.isFinite(+from)||!Number.isFinite(+until)||+from>=+until||until-from>7*86400000) throw Error('请选择有效的时间范围，最多 7 天。');
    return {site:currentSite===null?'':currentSite,stage:qualityNode('stage').value.trim(),rule_id:qualityNode('rule').value.trim(),version:qualityNode('version').value.trim(),from:from.toISOString(),until:until.toISOString()};
}
async function qualityRead(path,query,current) {
    var resp=await apiFetch('/api/quality/'+path+'?'+new URLSearchParams(query),{current:current,rawHTTPError:true});
    if (!current()) return null;
    if (!resp) throw Error('质量数据未能读取，请重试。');
    if (!resp.ok) throw Error(await resp.text());
    var data=await resp.json();return current()?data:null;
}
async function queryQuality(reset) {
    var f,mode=qualityNode('mode').value;
    try {f=reset?qualityFilters():qualityView.filter;if(!f) return;} catch(e) {editQuality();qualityNode('list').textContent=e.message;return;}
    if(!reset&&!qualityView.next) return;
    if(!reset&&f.site!==(currentSite===null?'':currentSite)) return queryQuality(true);
    var query=Object.assign({},f);
    if(!reset) {if(mode==='observations'){query.before=qualityView.next;query.cutoff=qualityView.cutoff;}else query.before_report=qualityView.next;}
    invalidateQuality();var seq=qualityView.seq,epoch=authGeneration,site=currentSite;
    var current=function(){return seq===qualityView.seq&&epoch===authGeneration&&site===currentSite&&currentModule==='quality';};
    qualityView.filter=f;qualityView.items=[];qualityView.report=null;qualityView.observation=null;
    qualityNode('list').textContent='正在读取质量记录…';qualityNode('detail').innerHTML='';qualityNode('next').disabled=true;
    try {
        var data=await qualityRead(mode,query,current);if(!data) return;
        qualityView.items=data.items||[];qualityView.next=data.next&&data.next!=='0'?String(data.next):'';qualityView.cutoff=data.cutoff||'';
        qualityNode('next').disabled=!qualityView.next;
        var meta='本页候选 '+data.scanned+' 条，匹配 '+qualityView.items.length+' 条。'+(mode==='observations'?'观测序号上界 #'+qualityView.cutoff+'；跨页不是事务快照。':'报告按 ID 翻页，列表实时读取。')+' 新增记录请更新结束时间后重新查询，或使用“最近 24 小时”。';
        qualityNode('list').innerHTML='<p class="hint">'+esc(meta)+'</p>'+(qualityView.items.length?qualityTable(mode):'<p class="quality-empty">本页没有匹配记录。可能尚未接入 Collector、没有观测、筛选不匹配或已过保留期。'+(qualityView.next?'可以继续扫描下一页。':'')+'</p>');
    } catch(e) {if(current())qualityNode('list').textContent=e.message;}
}
function qualityScope(s) {
    return esc(s.site)+' / '+esc(s.stage)+' · '+esc(s.rule_id)+' @ '+esc(s.version)+'<br><small>发布 '+esc(s.release_id||'未绑定')+' · 代次 '+esc(s.release_generation||'0')+'</small>';
}
var qualityVerdicts={insufficient:'样本不足',partial:'观测不完整',observed_ok:'已记录范围内未见异常',suspected_rule_drift:'疑似规则漂移',upstream_failure:'上游失败',processing_failure:'处理失败'};
function qualityTable(mode) {
    var rows=qualityView.items.map(function(item,i){var report=mode==='reports',s=report?item.query.scope:item.scope;
        return '<tr><td>'+qualityScope(s)+'</td><td>'+esc(new Date(item.created_at).toLocaleString())+'</td><td>'+esc(report?(qualityVerdicts[item.verdict]||item.verdict):(item.failure_kind||'无失败分类'))+'</td><td>'+esc(report?item.metrics.attempts:item.raw_rows)+'</td><td><button class="btn" data-quality-action="inspect" data-quality-index="'+i+'">'+(report?'查看报告':'查看观测')+'</button></td></tr>';
    }).join('');
    return '<div class="quality-table-wrap"><table class="schedule-table"><thead><tr><th>规则 / 发布范围</th><th>记录时间</th><th>判定 / 失败分类</th><th>'+(mode==='reports'?'尝试数':'原始行数')+'</th><th>详情</th></tr></thead><tbody>'+rows+'</tbody></table></div>';
}
function qualityDetailCurrent() {
    var seq=++qualityView.detailSeq,epoch=authGeneration,site=currentSite;
    qualityView.report=null;qualityView.observation=null;
    return function(){return seq===qualityView.detailSeq&&epoch===authGeneration&&site===currentSite&&currentModule==='quality';};
}
async function inspectQuality(id,reportID,isReport) {
    var current=qualityDetailCurrent();qualityNode('detail').textContent='正在校验并读取详情…';
    try {var data=await qualityRead(isReport?'report':'observation',isReport?{id:id}:{id:id,report_id:reportID||''},current);if(!data)return;
        if(isReport){qualityView.report=data;qualityNode('detail').innerHTML=qualityReport(data,true);}else{qualityView.observation=data;qualityNode('detail').innerHTML=qualityObservation(data);}
        qualityNode('detail').focus();
    }catch(e){if(current())qualityNode('detail').textContent=e.message;}
}
function qualityObservation(o) {
    return '<h3>观测 · '+esc(o.id)+'</h3><p>'+qualityScope(o.scope)+'</p><p>第 '+esc((o.attempt||0)+1)+' 次尝试（索引 '+esc(o.attempt||0)+'） · 操作 '+esc(o.operation)+' · HTTP '+esc(o.http_status||'未取得')+' · '+esc(o.failure_kind||'无失败分类')+'</p>'+
        '<div class="quality-actions"><button class="btn btn-primary" data-quality-action="summary">汇总此规则与发布范围</button>'+(o.task_id?'<button class="btn" data-quality-action="trace">查看任务 #'+esc(o.task_id)+' 的 Trace</button>':'')+'</div>'+
        '<p class="hint">汇总使用当前筛选时间与默认策略，按序号取最早 1000 条；截断时不代表全部。不保存为报告。Trace 按任务展示，请核对尝试编号；可能未开启或已清理。</p><dl class="quality-evidence"><dt>页面证据 ID</dt><dd>'+esc(o.evidence_id||'未关联')+'</dd><dt>原始响应 SHA-256</dt><dd>'+esc(o.body_sha256||'未记录')+'</dd></dl><p class="hint">这里只展示历史关联标识，不代表正文文件仍存在；失败现场可在 Trace 的归档步骤中查看。</p>'+qualityFields(o.fields||[])+'<details><summary>完整观测（不含页面正文与字段值）</summary><pre class="rule-json">'+esc(JSON.stringify(o,null,2))+'</pre></details>';
}
async function summarizeQuality() {
    var o=qualityView.observation;if(!o||!qualityView.filter)return;
    var query=Object.assign({},o.scope,{from:qualityView.filter.from,until:qualityView.filter.until});
    var current=qualityDetailCurrent();qualityNode('detail').textContent='正在计算临时汇总…';
    try{var data=await qualityRead('summary',query,current);if(!data)return;qualityView.report=data;qualityNode('detail').innerHTML=qualityReport(data,false);qualityNode('detail').focus();}
    catch(e){if(current())qualityNode('detail').textContent=e.message;}
}
function qualityFields(fields) {
    if(!fields.length)return '<p class="hint">没有可用字段统计。</p>';
    var rows=fields.map(function(f){return '<tr><td>'+esc(f.name)+'<br><small>'+esc(f.kind)+(f.required?' · 必填':'')+'</small></td><td>'+esc(f.rows)+'</td><td>'+esc(f.present)+'</td><td>'+esc(f.null)+'</td><td>'+esc(f.non_empty)+'</td><td>'+esc(f.invalid)+'</td></tr>';}).join('');
    return '<div class="quality-table-wrap"><table class="schedule-table"><thead><tr><th>字段</th><th>分母行数</th><th>存在</th><th>空值</th><th>非空</th><th>无效</th></tr></thead><tbody>'+rows+'</tbody></table></div>';
}
function qualityReport(r,persisted) {
    var m=r.metrics,stats=[['记录尝试',m.attempts],['折叠页面',m.pages],['完整响应评估页',m.evaluated_pages],['提取失败页',m.extraction_failures],['原始 / 新增 / 无效行',m.raw_rows+' / '+m.new_rows+' / '+m.invalid_rows]];
    var refs=(r.evidence||[]).map(function(ref,i){return '<li><button class="btn" data-quality-action="evidence" data-quality-index="'+i+'">观测 '+esc(ref.observation_id)+'</button><span>证据 '+esc(ref.evidence_id||'未关联')+'</span></li>';}).join('');
    return '<h3>'+(persisted?'持久报告 · '+esc(r.id):'临时汇总 · 未保存')+'</h3><p>'+qualityScope(r.query.scope)+'</p><p class="quality-verdict">'+esc(qualityVerdicts[r.verdict]||r.verdict)+(r.truncated?' · 输入已截断':'')+'</p><p class="hint">仅覆盖已记录观测；未记录的任务与失败写入量未知，不代表全站成功率。相同逻辑键保留最后一条，尝试失败分类仍统计每次尝试。</p><p>窗口 ['+esc(r.query.from)+'，'+esc(r.query.until)+') · 截止序号 '+esc(r.cutoff)+'</p>'+
        '<div class="quality-metrics">'+stats.map(function(s){return '<div><span>'+esc(s[0])+'</span><strong>'+esc(s[1])+'</strong></div>';}).join('')+'</div>'+qualityFields(m.fields||[])+
        '<details open><summary>失败分类与判定信号</summary><pre class="rule-json">'+esc(JSON.stringify({failures:m.failure_counts,signals:r.signals||[]},null,2))+'</pre></details>'+
        '<details><summary>策略、覆盖与输入摘要</summary><pre class="rule-json">'+esc(JSON.stringify({policy:r.policy,baseline_id:r.baseline_id||'',coverage:r.coverage,folding:r.folding,input_sha256:r.input_sha256,field_schema_sha256:r.field_schema_sha256,lost_observations:r.query.lost_observations},null,2))+'</pre></details>'+
        '<h4>证据关联 <small>最多保留 64 个引用</small></h4><p class="hint">持久报告独立于观测保留期。点击时重新校验关联；历史观测或正文可能已清理。</p><ul class="quality-refs">'+refs+'</ul>';
}
document.addEventListener('click',function(e){
    var b=e.target.closest('[data-quality-action]');if(!b||currentModule!=='quality')return;
    var action=b.dataset.qualityAction,index=Number(b.dataset.qualityIndex);
    if(action==='inspect'){var item=qualityView.items[index];if(item)inspectQuality(item.id,'',qualityNode('mode').value==='reports');}
    if(action==='summary')summarizeQuality();
    if(action==='evidence'){var report=qualityView.report,ref=report&&(report.evidence||[])[index];if(ref)inspectQuality(ref.observation_id,report.id||'',false);}
    if(action==='trace'&&qualityView.observation&&typeof showTaskTrace==='function'){
        var seq=qualityView.detailSeq;showTaskTrace(qualityView.observation.task_id,function(){return currentModule==='quality'&&seq===qualityView.detailSeq;});
    }
});
