/* Durable rule operations. Reads never publish; confirmed writes are not replayed automatically. */
'use strict';
var ruleOps = { listSeq:0, detailSeq:0, releaseSeq:0, previewSeq:0, after:null, pages:[], next:null, before:'0', releasePages:[], releaseNext:'0', current:null, preview:null, pending:null, busy:false };
var rulePendingKey = 'papa-rule-operation-v1';
function ruleNode(id) { return document.getElementById('rule-' + id); }
function ruleMessage(id, message) { ruleNode(id).innerHTML = '<p class="empty" role="status">' + esc(message) + '</p>'; }
function ruleJSON(value) { return '<pre class="rule-json">' + esc(JSON.stringify(value,null,2)) + '</pre>'; }
function ruleAction(label,action,attrs) { return '<button type="button" class="btn" data-rule-action="' + action + '"' + (attrs || '') + '>' + label + '</button>'; }
function ruleTime(value) { return value ? new Date(value).toLocaleString('zh-CN',{hour12:false}) : '—'; }
function ruleState(value){return ({draft:'待验收',pending:'验收未通过',verified:'验收通过',published:'曾发布'})[value]||value;}
function ruleOperationID(){
    if(typeof crypto.randomUUID==='function')return 'oa-'+crypto.randomUUID();
    var bytes=new Uint8Array(16);crypto.getRandomValues(bytes);
    return 'oa-'+Array.from(bytes,function(b){return b.toString(16).padStart(2,'0');}).join('');
}
function invalidateRules() { ruleOps.listSeq++; ruleOps.detailSeq++; ruleOps.releaseSeq++; invalidateRulePreview(); }
function resetRulesSession() {
    invalidateRules();
    Object.assign(ruleOps, { after:null, pages:[], next:null, before:'0', releasePages:[], releaseNext:'0', current:null, preview:null, pending:null, busy:false });
    ['versions','detail','releases','preview','result','pending'].forEach(function(id){var el=ruleNode(id);if(el)el.innerHTML='';});
    ['detail','preview','pending'].forEach(function(id){var el=ruleNode(id);if(el)el.hidden=true;});
    ['site','filter','bindings'].forEach(function(id){var el=ruleNode(id);if(el)el.value='';});
    ['versions-next','versions-prev','history-next','history-prev','publish-preview','rollback-preview'].forEach(function(id){var el=ruleNode(id);if(el)el.disabled=true;});
    var generation=ruleNode('generation');if(generation)generation.textContent='—';
}
function invalidateRulePreview() {
    ruleOps.previewSeq++; ruleOps.preview=null;
    if (ruleNode('preview')) ruleNode('preview').hidden=true;
}
function ruleSite() { return ruleNode('site').value.trim(); }
function restoreRulePending() {
    if (ruleOps.pending) return;
    try { var raw=sessionStorage.getItem(rulePendingKey); if (raw) { var p=JSON.parse(raw); if (!p || typeof p.operation_id!=='string' || !p.site || !['publish','rollback'].includes(p.kind)) throw new Error('invalid'); ruleOps.pending=p; } }
    catch (_) { ruleMessage('result','无法读取本页操作恢复记录，请先核对发布历史，避免重复操作。'); ruleOps.busy=true; }
}
function renderRulePending() {
    var p=ruleOps.pending,el=ruleNode('pending'); el.hidden=!p;
    if (p) el.innerHTML='<h3>待核对的人工操作</h3><p>' + esc(p.site) + ' · ' + (p.kind==='publish'?'发布':'回滚') + ' · 基于代次 ' + esc(p.generation) + '</p><p class="rule-id">操作 ID：' + esc(p.operation_id) + '</p><p class="hint">超时不代表失败。先核对结果；若未找到，可人工重发同一操作。刷新页面会保留此记录，不会自动重发。</p>' + ruleAction('核对结果','check') + ' ' + ruleAction('重发同一操作','resend');
    el.querySelectorAll('button').forEach(function(b){b.disabled=ruleOps.busy;});
    ruleNode('publish-preview').disabled=ruleOps.busy||!!p||!ruleSite()||ruleNode('generation').textContent==='—';
    ruleNode('rollback-preview').disabled=ruleOps.busy||!!p||!ruleOps.current||!ruleOps.current.previous_release_id;
}
function loadRules() {
    invalidateRules(); restoreRulePending(); renderRulePending();
    ruleNode('site').value=currentSite===null?'':currentSite;
    loadRuleVersions(true); loadRuleReleases(true);
}
async function loadRuleVersions(reset) {
    if(reset){ruleOps.after=null;ruleOps.pages=[];}
    var seq=++ruleOps.listSeq,epoch=authGeneration,id=ruleNode('filter').value.trim(),after=ruleOps.after;
    var current=function(){return seq===ruleOps.listSeq&&epoch===authGeneration&&currentModule==='rules';};
    ruleNode('versions-next').disabled=true;ruleNode('versions-prev').disabled=true;ruleMessage('versions','正在读取全局规则版本…');
    var url='/api/rules/versions?id='+encodeURIComponent(id);
    if(after)url+='&after_id='+encodeURIComponent(after.id)+'&after_version='+encodeURIComponent(after.version);
    try {
        var resp=await apiFetch(url,{current:current});if(!current())return;
        if(!resp){ruleMessage('versions','版本读取失败或未启用，请查看错误提示后刷新。');return;}
        var data=await resp.json();if(!current())return;
        ruleOps.next=data.next;
        ruleNode('versions').innerHTML=data.items.length?'<div class="schedule-scroll"><table class="schedule-table"><thead><tr><th>规则 / 版本</th><th>状态</th><th>创建时间</th><th>审阅</th></tr></thead><tbody>'+data.items.map(function(v){return '<tr><td><strong>'+esc(v.id)+'</strong><br>'+esc(v.version)+'</td><td>'+esc(ruleState(v.state))+(v.ever_published?' · 曾发布':'')+'</td><td>'+esc(ruleTime(v.created_at))+'</td><td>'+ruleAction('查看规则与验收','inspect',' data-id="'+esc(v.id)+'" data-version="'+esc(v.version)+'"')+'</td></tr>';}).join('')+'</tbody></table></div>':'<p class="empty">未找到规则版本。候选与样本验收由现有规则流程创建。</p>';
        ruleNode('versions-prev').disabled=!ruleOps.pages.length;ruleNode('versions-next').disabled=!ruleOps.next;
    }catch(_){if(current())ruleMessage('versions','版本响应不可用，请刷新重试。');}
}
function pageRuleVersions(next) { if(next){if(!ruleOps.next)return;ruleOps.pages.push(ruleOps.after);ruleOps.after=ruleOps.next;}else ruleOps.after=ruleOps.pages.pop()||null;loadRuleVersions(false); }
async function inspectRule(id,version,proofID) {
    var seq=++ruleOps.detailSeq,epoch=authGeneration,current=function(){return seq===ruleOps.detailSeq&&epoch===authGeneration&&currentModule==='rules';};
    ruleNode('detail').hidden=false;ruleMessage('detail','正在读取审阅依据…');
    var url=proofID?'/api/rules/verification?id='+encodeURIComponent(proofID):'/api/rules/version?id='+encodeURIComponent(id)+'&version='+encodeURIComponent(version);
    try{var resp=await apiFetch(url,{current:current});if(!current())return;if(!resp){ruleMessage('detail','审阅内容读取失败，请重试。');return;}var data=await resp.json();if(!current())return;
        ruleNode('detail').innerHTML=proofID?'<h3>发布时固定的验收报告</h3>'+ruleJSON(data):'<h3>'+esc(id)+' @ '+esc(version)+'</h3><p>规则摘要：<span class="rule-id">'+esc(data.version.Digest)+'</span></p><details open><summary>规则内容</summary>'+ruleJSON(data.version.Rule)+'</details><details open><summary>当前可用于发布的验收 · '+(data.verification?'通过':'无（未验收或最近验收未通过）')+'</summary>'+ruleJSON(data.verification)+'</details><details><summary>最近验收记录（最多 5 条，按创建时间倒序）</summary>'+ruleJSON(data.history||[])+'</details>';
        ruleNode('detail').innerHTML=ruleAction('收起审阅内容','close-detail')+ruleNode('detail').innerHTML;
        ruleNode('detail').scrollIntoView({block:'start'});ruleNode('detail').focus();
    }catch(_){if(current())ruleMessage('detail','审阅响应不可用，请重试。');}
}
function ruleSiteChanged(){invalidateRulePreview();loadRuleReleases(true);}
function editRuleSite(){ruleOps.releaseSeq++;ruleOps.current=null;ruleNode('generation').textContent='—';invalidateRulePreview();renderRulePending();}
async function loadRuleReleases(reset) {
    invalidateRulePreview();ruleOps.current=null;renderRulePending();
    if(reset){ruleOps.before='0';ruleOps.releasePages=[];}
    var seq=++ruleOps.releaseSeq,epoch=authGeneration,site=ruleSite(),current=function(){return seq===ruleOps.releaseSeq&&epoch===authGeneration&&currentModule==='rules'&&site===ruleSite();};
    ruleNode('history-next').disabled=true;ruleNode('history-prev').disabled=true;ruleNode('generation').textContent='—';renderRulePending();
    if(!site){ruleMessage('releases','选择上方站点，或填写目标站点后读取发布记录。');return;}
    ruleMessage('releases','正在读取当前发布与历史…');
    try{var resp=await apiFetch('/api/rules/releases?site='+encodeURIComponent(site)+'&before='+ruleOps.before,{current:current});if(!current())return;if(!resp){ruleMessage('releases','发布读取失败，请刷新后再操作。');return;}
        var data=await resp.json();if(!current())return;ruleOps.current=data.current;ruleOps.releaseNext=data.next;ruleNode('generation').textContent=data.current?data.current.generation:'0';
        var active=data.current;
        ruleNode('releases').innerHTML='<h3>当前发布</h3>'+(active?'<p class="rule-id">'+esc(active.id)+' · 代次 '+esc(active.generation)+'</p>'+ruleBindingHTML(active):'<p>尚未发布。首次发布基于代次 0。</p>')+'<h3>发布历史</h3>'+ (data.items.length?'<div class="schedule-scroll"><table class="schedule-table"><thead><tr><th>代次 / 动作</th><th>操作人 / 时间</th><th>绑定与固定验收</th></tr></thead><tbody>'+data.items.map(function(row){return '<tr><td>'+esc(row.generation)+' · '+(row.kind==='rollback'?'回滚':'发布')+'<details><summary>标识</summary><p class="rule-id">发布：'+esc(row.id)+'<br>操作：'+esc(row.operation_id)+'</p></details></td><td>'+esc(row.actor.operator)+'<br>'+esc(ruleTime(row.created_at))+'</td><td>'+ruleBindingHTML(row)+'</td></tr>';}).join('')+'</tbody></table></div>':'<p class="empty">暂无发布历史</p>');
        ruleNode('history-prev').disabled=!ruleOps.releasePages.length;ruleNode('history-next').disabled=data.next==='0';renderRulePending();
    }catch(_){if(current())ruleMessage('releases','发布响应不可用，请刷新重试。');}
}
function ruleBindingHTML(release){return Object.keys(release.bindings).sort().map(function(stage){var ref=release.bindings[stage];return '<div class="rule-binding"><strong>'+esc(stage)+'</strong> → '+esc(ref.id)+' @ '+esc(ref.version)+' '+ruleAction('固定验收','proof',' data-id="'+esc(release.verification_ids[stage])+'"')+'</div>';}).join('');}
function pageRuleHistory(next){if(next){if(ruleOps.releaseNext==='0')return;ruleOps.releasePages.push(ruleOps.before);ruleOps.before=ruleOps.releaseNext;}else ruleOps.before=ruleOps.releasePages.pop()||'0';loadRuleReleases(false);}
function fillRuleBindings(){if(!ruleOps.current)return;ruleNode('bindings').value=JSON.stringify(ruleOps.current.bindings,null,2);invalidateRulePreview();}
function ruleFormKey(){return JSON.stringify([ruleSite(),ruleNode('generation').textContent,ruleNode('bindings').value]);}
async function previewRuleChange(kind){
    if(ruleOps.busy||ruleOps.pending)return;
    invalidateRulePreview();var seq=ruleOps.previewSeq,epoch=authGeneration,key=ruleFormKey();
    var current=function(){return seq===ruleOps.previewSeq&&epoch===authGeneration&&currentModule==='rules'&&key===ruleFormKey();};
    var plan={kind:kind,site:ruleSite(),generation:ruleNode('generation').textContent};
    try{if(kind==='publish')plan.bindings=JSON.parse(ruleNode('bindings').value);}catch(_){Toast.err('完整阶段清单不是有效 JSON');return;}
    ruleNode('preview').hidden=false;ruleMessage('preview','正在核对规则与验收依据…');
    try{var resp=await apiRequest('/api/rules/preview',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(plan),current:current});if(!current())return;if(!resp){ruleMessage('preview','预览未通过，请修正清单或刷新发布记录。');return;}
        var data=await resp.json();if(!current())return;ruleOps.preview={data:data,key:key,epoch:epoch,seq:seq};
        var target=data.target?data.target.bindings:plan.bindings,prior=data.current?data.current.bindings:{};
        var removed=Object.keys(prior).filter(function(stage){return !Object.prototype.hasOwnProperty.call(target,stage);});
        ruleNode('preview').innerHTML='<h3>'+(kind==='rollback'?'回滚预览':'完整站点发布预览')+' · '+esc(plan.site)+'</h3><p>基于代次 '+esc(plan.generation)+(data.target?' · 回到发布 '+esc(data.target.id):'')+'</p><p class="hint">只影响后续取用新发布的采集；已经固定版本的在途批次不变。</p><p>移除阶段：'+esc(removed.length?removed.join('、'):'无')+'</p>'+data.stages.map(function(s){return '<details><summary>'+esc(s.stage)+' → '+esc(s.version.id)+' @ '+esc(s.version.version)+' · 验收通过</summary><p class="rule-id">规则摘要：'+esc(s.version.Digest)+'<br>验收 ID：'+esc(s.verification.ID)+'<br>样本集：'+esc(s.verification.SetID)+'</p>'+ (s.comparison?ruleJSON(s.comparison):'<p>新增阶段绑定</p>')+ruleJSON(s.version.Rule)+ruleJSON(s.verification)+'</details>';}).join('')+'<label class="rule-ack"><input type="checkbox" id="rule-reviewed"> 已核对全部阶段、移除项及验收依据</label>'+ruleAction('确认'+(kind==='rollback'?'回滚':'发布'),'confirm');
    }catch(_){if(current())ruleMessage('preview','预览响应不可用，请重新预览。');}
}
async function confirmRuleChange(){
    var review=ruleOps.preview;
    if(ruleOps.busy||ruleOps.pending||!review||!ruleNode('reviewed').checked){Toast.err('请先预览并勾选审阅确认');return;}
    ruleOps.busy=true;renderRulePending();
    try{
        var p=review.data.plan;
        if(!await Dialog.confirm({title:'人工'+(p.kind==='publish'?'发布':'回滚')+' · '+p.site,body:esc('基于代次 '+p.generation+' 提交完整站点变更。已固定的采集批次保持原版本。')}))return;
        if(currentModule!=='rules'||review!==ruleOps.preview||review.epoch!==authGeneration||review.key!==ruleFormKey())return;
        var request=JSON.parse(JSON.stringify(p));request.operation_id=ruleOperationID();
        if(p.kind==='publish')request.verification_ids=review.data.verification_ids;
        // Persist before dispatch. A storage failure must prevent the write.
        sessionStorage.setItem(rulePendingKey,JSON.stringify(request));ruleOps.pending=request;invalidateRulePreview();
        await sendRulePending();
    }catch(_){if(review.epoch===authGeneration)Toast.err('无法保存操作恢复记录，未确认提交；请核对待处理操作。');}
    finally{if(review.epoch===authGeneration){ruleOps.busy=false;renderRulePending();}}
}
function finishRulePending(data,p){
    if(!data||data.operation_id!==p.operation_id||data.site!==p.site)throw new Error('operation mismatch');
    sessionStorage.removeItem(rulePendingKey);ruleOps.pending=null;
    ruleMessage('result','操作已确认：'+p.site+' · 代次 '+data.generation+' · 发布 '+data.id);
    if(currentModule==='rules'&&ruleSite()===p.site){loadRuleReleases(true);ruleNode('result').scrollIntoView({block:'center'});ruleNode('result').focus();}
}
async function sendRulePending(){
    var p=ruleOps.pending,epoch=authGeneration;if(!p)return;
    var resp=await apiRequest('/api/rules/change',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p),rawHTTPError:true});
    if(!resp||epoch!==authGeneration||ruleOps.pending!==p)return;
    if(resp.ok){var data=await resp.json();if(epoch===authGeneration&&ruleOps.pending===p)finishRulePending(data,p);}
    else{
        var message=await responseError(resp);if(epoch!==authGeneration||ruleOps.pending!==p)return;
        // These statuses are returned only before a change or after a rolled
        // back transaction. Network/5xx failures always keep the saved ID.
        if(resp.status===403||resp.headers.get('X-Rule-Outcome')==='rejected'){sessionStorage.removeItem(rulePendingKey);ruleOps.pending=null;invalidateRulePreview();}
        Toast.err(message);ruleMessage('result',message);
    }
}
async function resolveRulePending(resend){
    if(ruleOps.busy||!ruleOps.pending)return;
    var p=ruleOps.pending,epoch=authGeneration;ruleOps.busy=true;renderRulePending();
    try{
        if(resend){if(!await Dialog.confirm({title:'重发同一操作',body:esc('站点 '+p.site+'，操作 ID '+p.operation_id+'。参数和代次保持原样；不会创建新的发布请求。')}))return;if(epoch!==authGeneration||ruleOps.pending!==p||currentModule!=='rules')return;await sendRulePending();}
        else{var resp=await apiFetch('/api/rules/operation?id='+encodeURIComponent(p.operation_id),{rawHTTPError:true});if(epoch!==authGeneration||ruleOps.pending!==p||!resp)return;if(resp.ok){var data=await resp.json();if(epoch===authGeneration&&ruleOps.pending===p)finishRulePending(data,p);}else{Toast.err(await responseError(resp));}}
    }catch(_){if(epoch===authGeneration)Toast.err('结果仍未确认，已保留原操作 ID。');}
    finally{if(epoch===authGeneration){ruleOps.busy=false;renderRulePending();}}
}
document.addEventListener('click',function(event){var b=event.target.closest('[data-rule-action]');if(!b||currentModule!=='rules'||b.disabled)return;
    var a=b.dataset.ruleAction;if(a==='inspect')inspectRule(b.dataset.id,b.dataset.version);else if(a==='proof')inspectRule('','',b.dataset.id);else if(a==='close-detail'){ruleOps.detailSeq++;ruleNode('detail').hidden=true;ruleNode('filter').focus();}else if(a==='confirm')confirmRuleChange();else if(a==='check')resolveRulePending(false);else if(a==='resend')resolveRulePending(true);
});
