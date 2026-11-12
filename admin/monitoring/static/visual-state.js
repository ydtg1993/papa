/* monitoring/visual-state: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("monitoring/visual-state","dashboard",[],function(deps,lifecycle){
'use strict';
/* Bounded, page-local observations. No persistent history and no rate inference. */
'use strict';
var MonitorObservations = (function () {
    var samples = [], instance = null;
    function time(value) { var n = typeof value === 'string' ? Date.parse(value) : NaN; return Number.isFinite(n) && n > 946684800000 ? n : null; }
    function count(n) { return Number.isSafeInteger(n) && n >= 0 ? n : null; }
    function reset() { samples = []; instance = null; }
    function accept(data) {
        var at = time(data.generated_at), id = data.instance_id;
        if (at === null || typeof id !== 'string' || !id) { reset(); return false; }
        if (id !== instance || (samples.length && at < samples[samples.length - 1].at)) reset();
        instance = id;
        if (samples.length && at === samples[samples.length - 1].at) return false;
        var stages = Object.keys(data.stages || {}).sort().map(function (key) {
            var s = data.stages[key] || {}, q = s.queue || {};
            return { key: key, site: s.site, stage: s.stage || key, queued: count(q.queue_len), unfinished: count(q.in_progress), completed: count(q.completed), failed: count(q.failed) };
        });
        samples.push({ at: at, stages: stages, system: Object.assign({}, data.system || {}) });
        samples = samples.filter(function (s) { return at - s.at <= 600000; }).slice(-240);
        return true;
    }
    function selected(sample, site) { return sample.stages.filter(function (s) { return site === null || s.site === site; }); }
    function sum(stages, field) {
        if (!stages.length) return null;
        var total = 0;
        for (var s of stages) { if (s[field] === null || count(total + s[field]) === null) return null; total += s[field]; }
        return total;
    }
    function trend(site, field) {
        var points = [], prev = null, signature = null;
        samples.forEach(function (sample) {
            var stages = selected(sample, site), keys = JSON.stringify(stages.map(function (s) { return [s.key, s.site]; }));
            if (prev && (sample.at - prev > 15000 || keys !== signature)) points.push({ x: prev + (sample.at - prev) / 2, y: null });
            points.push({ x: sample.at, y: sum(stages, field) });
            prev = sample.at; signature = keys;
        });
        // Break markers share the chart's 300 point budget with observations.
        return points.slice(-300);
    }
    function resource(system, key, now) {
        system = system || {};
        var at = time(system[key + '_sampled_at']), value = system[key + '_percent'];
        var interval = Number(system.sample_interval_ms), limit = Number.isFinite(interval) && interval > 0 ? Math.max(15000, interval * 3) : 15000;
        if (at === null) return { value: null, at: null, state: '尚无成功采样' };
        if (system[key + '_available'] !== true) return { value: null, at: at, state: '采样失败' };
        if (!Number.isFinite(value) || value < 0 || value > 100) return { value: null, at: at, state: '数值不可用' };
        if (now === null || at > now || now - at > limit) return { value: null, at: at, state: '样本过期' };
        return { value: value, at: at, state: '可用' };
    }
    function resourceTrend(key) {
        var points = [], previous = null, lastPoll = null, pendingBreak = false;
        samples.forEach(function (s) {
            var value = resource(s.system, key, s.at), x = value.value === null ? s.at : value.at;
            var disconnected = lastPoll !== null && s.at - lastPoll > 15000;
            pendingBreak = pendingBreak || disconnected;
            lastPoll = s.at;
            if (previous !== null && x <= previous) return;
            if (pendingBreak && points.length) points.push({ x: previous + (x - previous) / 2, y: null });
            pendingBreak = false;
            points.push({ x: x, y: value.value }); previous = x;
        });
        return points.slice(-300);
    }
    return { accept: accept, reset: reset, trend: trend, resource: resource, resourceTrend: resourceTrend, time: time,
        size: function () { return samples.length; }, latest: function () { return samples[samples.length - 1] || null; } };
})();



function reset(){MonitorObservations.reset();}
return Object.defineProperties({}, {
reset:{enumerable:true,value:reset},
"MonitorObservations":{enumerable:true,get:function(){return MonitorObservations;},set:function(value){MonitorObservations=value;}}
});
});
MonitorShell.install("monitoring/visual-state");
