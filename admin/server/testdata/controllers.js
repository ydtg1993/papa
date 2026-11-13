// Load shipped controller factories, never copied/sliced implementation bodies.
const vm = require('node:vm');
function prepare(context) {
    const reads={getSessionGeneration:['authGeneration',0],getActiveModule:['currentModule','dashboard'],getSelectedSite:['currentSite',null],getMonitorSnapshot:['lastData',null],getCurrentTable:['currentTable',null],getCurrentPage:['currentPage',null],getNavigationSection:['currentSection','monitor'],getTableViewRevision:['tableViewEpoch',0],getWorkbenchConnection:['workbenchConnection',{updatedAt:null,failed:false,message:''}]};
    for(const [method,[property,fallback]] of Object.entries(reads))if(!context[method])context[method]=()=>context[property]===undefined?fallback:context[property];
    if(!context.selectMonitorSite)context.selectMonitorSite=value=>{context.currentSite=value;};
    if(!context.setActiveModule)context.setActiveModule=value=>{context.currentModule=value;};
    if(!context.nextTableView)context.nextTableView=()=>context.tableViewEpoch=(context.tableViewEpoch||0)+1;
}
// Test fixtures replace dependencies through their owning controller exports.
// Production page-operation properties remain read-only.
function fixtureOverrides(context, names) {
    for (const name of names) {
        const instance = context.MonitorShell.controller(name);
        if (!instance) continue;
        for (const key of Object.keys(instance.exports)) {
            const descriptor = Object.getOwnPropertyDescriptor(context, key);
            if (descriptor && descriptor.get && descriptor.configurable) Object.defineProperty(context, key, {
                ...descriptor, set(value) { instance.exports[key] = value; }
            });
        }
    }
}
function run(source, context) {
    prepare(context);
    const result = vm.runInContext(source,context);
    fixtureOverrides(context, Array.from(source.matchAll(/MonitorShell\.install\("([^"]+)"\)/g), match => match[1]));
    return result;
}
function load(context, source, names) {
    run(source.replace(/^MonitorShell\.install\([^\n]+\);\s*$/gm,''),context);
    for(const name of names)context.MonitorShell.install(name);
    fixtureOverrides(context, names);
}
module.exports={run,load,prepare};
