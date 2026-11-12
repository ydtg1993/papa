const controllers=require('./controllers');
const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const nodes={},charts=new Map(),windowEvents={},documentEvents={},animated=new Set(),chartRegions=[{hidden:false},{hidden:false},{hidden:false}];let created=0,destroyed=0;
function node(id){return nodes[id]||(nodes[id]={id,hidden:false,open:false,textContent:'',listeners:{},
    querySelectorAll:()=>id==='mod-dashboard'?chartRegions:[],addEventListener(type,listener){(this.listeners[type]||=([])).push(listener);},
    emit(type){for(const listener of this.listeners[type]||[])listener({target:this});}});}
const chartAPI={create(el,options){created++;charts.set(el.id,options);let alive=true;return{
    update(o){assert.ok(alive,'updated a destroyed chart');charts.set(el.id,o);},
    destroy(){assert.ok(alive,'destroyed a chart twice');alive=false;destroyed++;charts.delete(el.id);}};}};
const motionAPI={animate:el=>animated.add(el.id),cancel:el=>animated.delete(el.id)};
const c=vm.createContext({window:{addEventListener:(type,listener)=>windowEvents[type]=listener},document:{hidden:false,getElementById:node,addEventListener:(type,listener)=>documentEvents[type]=listener},currentSite:null,currentModule:'dashboard',
    Oao:{charts:chartAPI,motion:motionAPI}});
for(const script of ['workbench.js','visual-state.js','visuals.js'])controllers.run(fs.readFileSync(path.join(process.argv[2],script),'utf8'),c);
const data={instance_id:'one',generated_at:'2026-11-07T00:00:00Z',stages:{x:{site:'one',stage:'a',queue:{queue_len:0,in_progress:1,completed:8,failed:0}}}};
function toggle(id,open){const el=node(id);el.open=open;el.emit('toggle');}
function count(n){assert.equal(c.monitorChartCount(),n);assert.equal(charts.size,n);assert.equal(created-destroyed,n);}

// Real production handlers must keep unsampled / unaccepted data empty.
c.renderMonitorVisuals(null);count(0);
toggle('workbench-analysis',true);toggle('workbench-resources',true);count(0);assert.equal(c.MonitorObservations.size(),0);
toggle('workbench-analysis',false);toggle('workbench-resources',false);
c.acceptMonitorObservation(data);c.renderMonitorVisuals(data);count(1);
assert.ok(charts.get('visual-queue-trend').description.includes('不能相加'));
assert.equal(node('workbench-analysis').listeners.toggle.length,1);
assert.equal(node('workbench-resources').listeners.toggle.length,1);

// Disclosures reuse the accepted snapshot, including the most recent poll
// received while hidden. A UI action must never call accept again.
let accepted=0;const accept=c.acceptMonitorObservation;c.acceptMonitorObservation=d=>{accepted++;return accept(d);};
toggle('workbench-analysis',true);count(3);assert.equal(charts.get('visual-stage-bars').data[0].value,0);assert.equal(charts.get('visual-results').data[0].value,8);
toggle('workbench-resources',true);count(6);
assert.equal(nodes['visual-cpu-value'].textContent,'—');assert.match(nodes['visual-cpu-stamp'].textContent,/尚无成功采样/);
assert.match(charts.get('visual-cpu').title,/系统采样/);assert.match(charts.get('visual-disk').title,/根文件系统/);
assert.equal(c.MonitorObservations.size(),1);assert.equal(accepted,0);
toggle('workbench-analysis',false);count(4);assert.equal(destroyed,2);
toggle('workbench-resources',false);count(1);assert.equal(destroyed,5);assert.equal(animated.size,0,'hidden resource values retained animations');

const fresh={...data,generated_at:'2026-11-07T00:00:05Z',system:{cpu_percent:42,cpu_available:true,cpu_sampled_at:'2026-11-07T00:00:05Z'},
    stages:{x:{...data.stages.x,queue:{queue_len:4,in_progress:6,completed:12,failed:0}}}};
c.acceptMonitorObservation(fresh);c.renderMonitorVisuals(fresh);count(1);
assert.equal(animated.size,0,'collapsed resource values animated on a poll');
toggle('workbench-analysis',true);count(3);assert.equal(charts.get('visual-stage-bars').data[0].value,4);
toggle('workbench-resources',true);count(6);assert.equal(nodes['visual-cpu-value'].textContent,'42.0%');assert.ok(animated.has('visual-cpu-value'));
assert.equal(c.MonitorObservations.size(),2);assert.equal(accepted,1);
for(let i=0;i<4;i++)c.renderMonitorVisuals(fresh);
assert.equal(node('workbench-analysis').listeners.toggle.length,1);assert.equal(node('workbench-resources').listeners.toggle.length,1);
assert.equal(created,11,'polls created duplicate chart instances');

// Browser backgrounding can occur between the details state change and its
// queued toggle event. Do not create hidden instances; resume from the same
// observations immediately when visible, without another fetch or accept.
c.document.hidden=true;documentEvents.visibilitychange();assert.equal(animated.size,0);
toggle('workbench-analysis',false);toggle('workbench-resources',false);count(1);
toggle('workbench-resources',true);c.setMonitorVisualConnection(false);count(1);assert.equal(created,11);
c.document.hidden=false;documentEvents.visibilitychange();count(4);assert.equal(c.MonitorObservations.size(),2);assert.equal(accepted,1);
toggle('workbench-analysis',true);count(6);
c.currentSite='absent';c.renderMonitorVisuals(fresh);assert.equal(charts.get('visual-results').data.length,0);assert.equal(c.MonitorObservations.size(),2);

// Pending toggle, error refresh and pagehide cannot revive a hidden dashboard.
c.currentSite=null;c.destroyMonitorVisuals();c.currentModule='settings';count(0);assert.equal(destroyed,created);assert.equal(animated.size,0);
toggle('workbench-analysis',false);toggle('workbench-analysis',true);toggle('workbench-resources',true);c.setMonitorVisualConnection(false);count(0);
c.document.hidden=true;documentEvents.visibilitychange();c.document.hidden=false;documentEvents.visibilitychange();count(0);
c.currentModule='dashboard';c.renderMonitorVisuals(fresh);count(6);assert.match(nodes['visual-observation-meta'].textContent,/连接异常/);
windowEvents.pagehide();count(0);const hiddenCreated=created;toggle('workbench-resources',true);assert.equal(created,hiddenCreated,'pagehide was undone by a queued toggle');
c.renderMonitorVisuals(fresh);c.setMonitorVisualConnection(false);count(0);
c.resumeMonitorVisuals();count(6);
c.resetMonitorObservations();count(0);assert.equal(c.MonitorObservations.size(),0);assert.equal(animated.size,0);
for(const key of ['cpu','mem','disk']){assert.equal(nodes['visual-'+key+'-value'].textContent,'—');assert.equal(nodes['visual-'+key+'-stamp'].textContent,'尚无成功采样');}
assert.equal(c.getVisualSnapshot(),null);assert.equal(nodes['visual-observation-meta'].textContent,'等待新的监控快照');
toggle('workbench-resources',true);c.setMonitorVisualConnection(false);count(0);
c.document.hidden=true;documentEvents.visibilitychange();c.document.hidden=false;documentEvents.visibilitychange();count(0);

// A released oao without the optional charts extension preserves the fallback.
c.Oao={};c.acceptMonitorObservation(data);c.renderMonitorVisuals(data);count(0);
assert.ok(nodes['visual-availability'].textContent.includes('未包含'));assert.equal(nodes['visual-availability'].hidden,false);
assert.ok(chartRegions.every(region=>region.hidden),'legacy oao leaves empty chart regions visible in disclosures');
toggle('workbench-analysis',true);toggle('workbench-resources',true);count(0);assert.equal(c.MonitorObservations.size(),1);
c.Oao={charts:chartAPI,motion:motionAPI};
const legacy={stages:data.stages};c.acceptMonitorObservation(legacy);c.renderMonitorVisuals(legacy);count(6);
assert.ok(chartRegions.every(region=>!region.hidden));
assert.match(charts.get('visual-queue-trend').error,/无法建立可信趋势/);assert.equal(c.MonitorObservations.size(),0);
assert.equal(nodes['visual-cpu-value'].textContent,'—');

// Ranking still preserves totals, including a real key that resembles the
// synthetic remainder. Invalid inputs retain their explicit unknown state.
const many={...data,stages:Object.fromEntries(Array.from({length:55},(_,i)=>['s'+i,{site:'one',stage:'s'+i,queue:{queue_len:1,in_progress:1,completed:0,failed:0}}]))};
c.acceptMonitorObservation(many);c.renderMonitorVisuals(many);const bars=charts.get('visual-stage-bars');assert.equal(bars.data.length,12);assert.equal(bars.data.reduce((s,x)=>s+x.value,0),55);assert.match(bars.data.at(-1).label,/其他 44/);
many.stages.__papa_other__={site:'one',stage:'reserved-looking real name',queue:{queue_len:100,in_progress:100,completed:0,failed:0}};
c.resetMonitorObservations();c.acceptMonitorObservation(many);c.renderMonitorVisuals(many);
const collisionBars=charts.get('visual-stage-bars').data;assert.equal(new Set(collisionBars.map(b=>b.key)).size,12,'synthetic other key collides with a real stage');
many.stages.s0.queue.queue_len='unknown';c.renderMonitorVisuals(many);assert.match(charts.get('visual-stage-bars').error,/不完整/);
c.destroyMonitorVisuals();count(0);
console.log('visual assembly: progressive chart lifecycle, accepted snapshots, unknown values, fallback, ranking totals and cleanup passed');
