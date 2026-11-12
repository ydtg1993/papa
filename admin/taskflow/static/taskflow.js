/* Task-flow editor owns its mount state; the shell supplies transport and page lifecycle. */
MonitorShell.define("taskflow/editor","pages",["Papa","document","window","apiRequest"],function(deps,lifecycle){
deps.Papa.page('task-flow-policy', function(root) {
  let disposed = false, busy = false, snapshot = null, dirty = false;
  const el = (tag, text) => { const n = deps.document.createElement(tag); if (text) n.textContent = text; return n; };
  const host=root; root=el('section'); root.className = 'taskflow-panel'; host.append(root);
  const title = el('h3', '选择采集运行'); title.className='taskflow-title';
  const note = el('p', '查看当前调度，按阶段调整广度、深度与并发。策略切换不会中断正在执行的任务。'); note.className='taskflow-intro';
  const id = el('input'); id.className='taskflow-run-input'; id.placeholder = '输入调度运行 ID'; id.setAttribute('aria-label', '调度运行 ID');
  const load = el('button', '查询运行'); load.className='btn taskflow-query'; load.type='button';
  const message = el('p'); message.setAttribute('role', 'status'); message.className='taskflow-status';
  const editor = el('div'), list = el('div'); editor.className='taskflow-editor'; list.className='taskflow-results';
  const apply = el('button', '确认并应用策略'); apply.className='btn taskflow-apply'; apply.type='button'; apply.disabled = true; apply.hidden=true;
  root.append(title, note, id, load, message, editor, apply, list);
  const empty=el('div'); empty.className='taskflow-empty';
  const emptyTitle=el('h4','从一个运行开始'), emptyText=el('p','输入业务创建的运行 ID，查看各阶段的调度范围与任务等待原因。');
  const guide=el('div');guide.className='taskflow-guide';
  for(const [name,body] of [['广度优先','先处理同一层任务，适合目录发现。'],['分支深度优先','优先完成一条内容链，可调整并行分支数。']]){const block=el('div');block.append(el('strong',name),el('p',body));guide.append(block);}
  empty.append(emptyTitle,emptyText,guide);root.append(empty);
  const controls = new Map();
  async function request(url,options={}) {
    const response=await deps.apiRequest(url,{...options,rawHTTPError:true,headers:{'Content-Type':'application/json'},current:()=>!disposed});
    if (!response) throw new Error('请求结果尚未确认，请刷新核对');
    if (!response.ok) throw new Error(await response.text());
    return response.json();
  }
  function lock(value) { busy = value; root.setAttribute('aria-busy',String(value)); load.textContent=value?'正在处理…':snapshot?'刷新运行':'查询运行'; load.disabled = value; id.disabled = value; apply.disabled = value || !snapshot || !dirty || id.value.trim() !== snapshot.id; apply.hidden=!snapshot; for (const c of controls.values()) { c.mode.disabled = value || !snapshot; c.count.disabled = value || !snapshot; } }
  function render(p) {
    snapshot = p; dirty = false; empty.hidden=true; controls.clear(); editor.replaceChildren(); list.replaceChildren();
    message.textContent = `站点 ${p.site} · 策略 v${p.revision} · 活跃 ${p.active_count ?? p.tasks.length} 条，展示前 ${p.tasks.length} 条；状态以查询时为准`;
    for (const [key, lane] of Object.entries(p.policy.lanes)) {
      const row = el('p'), label = el('label', `${key}（${Object.entries(p.policy.stages).filter(([,v])=>v===key).map(([k])=>k).join(' → ')}） `);
      row.className='taskflow-lane';
      const mode = el('select'); for (const [v,t] of [['breadth','广度优先'],['depth','分支深度优先']]) { const o=el('option',t);o.value=v;mode.append(o); } mode.value=lane.mode;
      const count=el('input'); count.type='number'; count.min='1';count.max='64';count.value=lane.parallel; count.setAttribute('aria-label', `${key} 并发上限`);
      mode.onchange=count.oninput=()=>{dirty=true;lock(false);};label.append(mode);row.append(label,el('span',' 并发上限 '),count);editor.append(row);controls.set(key,{mode,count});
    }
    editor.append(el('p','深度模式的上限表示并行分支数，每分支一次执行一条任务；设为 1 即严格分支优先。广度模式的上限表示同层并行任务数。'));
    list.append(el('h4','任务状态'));
    if(!p.tasks.length){list.append(el('p','当前没有待处理或执行中的任务。'));return;}
    const table=el('table'), head=el('tr'); table.className='workbench-table'; for(const name of ['任务','阶段','分支','等待 / 执行原因'])head.append(el('th',name)); table.append(head);
    for(const t of p.tasks){const row=el('tr');for(const value of [t.id,t.stage,t.branch,t.reason])row.append(el('td',String(value)));table.append(row);}list.append(table);
  }
  async function refresh() {
    if (disposed || busy || (dirty && !deps.window.confirm('刷新会放弃尚未应用的策略编辑，继续？'))) return;
    const key=id.value.trim();if(!key){message.textContent='请先输入调度运行 ID。';return;}lock(true);
    try { const p=await request('/api/task-flow?id='+encodeURIComponent(key));if(!disposed)render(p); }
    catch(e){if(!disposed)message.textContent=e.message;}
    finally{if(!disposed)lock(false);}
  }
  load.onclick=refresh;
  id.onkeydown=event=>{if(event.key==='Enter'){event.preventDefault();refresh();}};
  id.oninput=()=>{lock(false);};
  apply.onclick=async()=>{
    if(disposed||busy||!snapshot||!dirty||id.value.trim()!==snapshot.id)return;
    const policy=structuredClone(snapshot.policy);
    for(const [key,c] of controls){const n=Number(c.count.value);if(!Number.isInteger(n)||n<1||n>64){message.textContent='并发上限须为 1–64 的整数';return;}policy.lanes[key]={mode:c.mode.value,parallel:n};}
    if(!deps.window.confirm('将新策略应用到当前运行的待执行任务？在途任务不会中断。'))return;
    lock(true);
    try{await request('/api/task-flow?id='+encodeURIComponent(snapshot.id),{method:'POST',body:JSON.stringify({revision:snapshot.revision,policy})});if(disposed)return;dirty=false; snapshot=null;message.textContent='策略已更新。请刷新查看实际运行状态。';}
    catch(e){if(!disposed)message.textContent=e.message+'；请刷新核对后再提交。';}
    finally{if(!disposed)lock(false);}
  };
  return {destroy(){disposed=true;},beforeLeave(){return !dirty||deps.window.confirm('策略编辑尚未应用，确定离开？');}};
});

return {};
});
MonitorShell.install("taskflow/editor");
