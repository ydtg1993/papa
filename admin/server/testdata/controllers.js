// Load shipped controller factories, never copied/sliced implementation bodies.
const vm = require('node:vm');
function prepare(context) {
    const reads={getSessionGeneration:['authGeneration',0],getActiveModule:['currentModule','dashboard'],getSelectedSite:['currentSite',null],getMonitorSnapshot:['lastData',null],getCurrentTable:['currentTable',null],getCurrentPage:['currentPage',null],getNavigationSection:['currentSection','monitor'],getTableViewRevision:['tableViewEpoch',0],getWorkbenchConnection:['workbenchConnection',{updatedAt:null,failed:false,message:''}]};
    for(const [method,[property,fallback]] of Object.entries(reads))if(!context[method])context[method]=()=>context[property]===undefined?fallback:context[property];
    if(!context.selectMonitorSite)context.selectMonitorSite=value=>{context.currentSite=value;};
    if(!context.setActiveModule)context.setActiveModule=value=>{context.currentModule=value;};
    if(!context.nextTableView)context.nextTableView=()=>context.tableViewEpoch=(context.tableViewEpoch||0)+1;
}
function run(source, context) {prepare(context);return vm.runInContext(source,context);}
function load(context, source, names) {
    run(source.replace(/^MonitorShell\.install\([^\n]+\);\s*$/gm,''),context);
    for(const name of names)context.MonitorShell.install(name);
}
module.exports={run,load,prepare};
