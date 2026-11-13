/* shell/table-adapter: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/table-adapter",null,["Oao","document"],function(deps,lifecycle){
'use strict';
var tableViewEpoch=0;
    function retireTableView() {
        tableViewEpoch++;
        var el = deps.document.getElementById('oao-view');
        if (el) deps.Oao.destroy(el);
    }

function nextTableView(){return ++tableViewEpoch;} function getTableViewRevision(){return tableViewEpoch;}

return Object.defineProperties({}, {
"retireTableView":{enumerable:true,get:function(){return retireTableView;},set:function(value){retireTableView=value;}},
"nextTableView":{enumerable:true,get:function(){return nextTableView;},set:function(value){nextTableView=value;}},
"getTableViewRevision":{enumerable:true,get:function(){return getTableViewRevision;},set:function(value){getTableViewRevision=value;}}
});
});
MonitorShell.install("shell/table-adapter");
