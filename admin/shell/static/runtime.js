/* Controller registry. The default instance publishes legacy callable names only
 * as a compatibility bridge; controller dependencies resolve against owners. */
(function (root) {
    'use strict';
    if (root.MonitorShell) return;
    var definitions = new Map();
    function create(environment, compatibility) {
        var instances = new Map(), owners = new Map(), active = 'dashboard';
        function resolve(name) {
            if(name==='MonitorShell') return api;
            var owner = owners.get(name);
            if(owner)return owner.exports[name];
            var value=environment[name];
            // Browser transport methods require their Window receiver even when
            // supplied through an explicit controller dependency object.
            return name==='fetch'&&typeof value==='function'?value.bind(environment.window||environment):value;
        }
        function install(name) {
            if (instances.has(name)) return instances.get(name);
            var definition = definitions.get(name);
            if (!definition) throw new Error('Unknown monitor controller: ' + name);
            var epoch = 0, disposed = false, deps = {}, cleanups = [];
            definition.dependencies.forEach(function (key) {
                Object.defineProperty(deps, key, { get: function () {
                    if (key === 'captureMonitorView') return function () {
                        var revision = epoch, capture = resolve(key), current = capture ? capture() : function () { return true; };
                        return function () { return !disposed && epoch === revision && current(); };
                    };
                    return resolve(key);
                }});
            });
            var lifecycle = { listen: function(target,type,handler){target.addEventListener(type,handler);cleanups.push(function(){if(target.removeEventListener)target.removeEventListener(type,handler);});}, cleanup:function(handler){cleanups.push(handler);}, invalidate: function () { epoch++; }, current: function () { var revision=epoch;return function(){return !disposed&&revision===epoch;}; } };
            var exports = definition.factory(deps, lifecycle);
            var instance = { exports: exports, mount: function () { disposed=false; if(exports.mount) exports.mount(); },
                unmount: function () { epoch++; if(exports.unmount)exports.unmount(); },
                reset: function () { epoch++; if(exports.reset)exports.reset(); },
                destroy: function () { epoch++;disposed=true;cleanups.splice(0).forEach(function(cleanup){cleanup();});if(exports.destroy)exports.destroy();else if(exports.unmount)exports.unmount(); } };
            instances.set(name, instance);
            Object.keys(exports).forEach(function (key) {
                if (['mount','unmount','reset','destroy'].indexOf(key)>=0) return;
                owners.set(key, instance);
                if (compatibility) {
                    var old=Object.getOwnPropertyDescriptor(compatibility,key);
                    if(!old||old.configurable)Object.defineProperty(compatibility,key,{configurable:true,enumerable:true,get:function(){return exports[key];},set:function(v){exports[key]=v;}});
                }
            });
            return instance;
        }
        var api = { install: install, get: function(name){return instances.get(name);},
            transition:function(next){instances.forEach(function(instance,name){var module=definitions.get(name).module;if(module===active)instance.unmount();});active=next;instances.forEach(function(instance,name){if(definitions.get(name).module===active)instance.mount();});},
            reset:function(){instances.forEach(function(instance){instance.reset();});},
            destroy:function(){instances.forEach(function(instance){instance.destroy();});},
            resolve:resolve };
        return api;
    }
    var host=create(root,root);
    root.MonitorShell={define:function(name,module,dependencies,factory){definitions.set(name,{module:module,dependencies:dependencies,factory:factory});},
        install:function(name){return host.install(name);},create:create,transition:host.transition,reset:host.reset,destroy:host.destroy,
        controller:function(name){return host.get(name);}};
})(globalThis);
