/* shell/appearance: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/appearance",null,["Oao","Papa","document","getTheme","localStorage","setTheme","window"],function(deps,lifecycle){
'use strict';
/* Browser-local display preferences; changing them never remounts a view. */
'use strict';
var MonitorAppearance = (function () {
    var key = 'papa-appearance', listeners = new Set(), root = deps.document.documentElement, unsaved = Object.create(null);
    function get() { return root.getAttribute('data-appearance') === 'classic' ? 'classic' : 'simple'; }
    function refresh() { listeners.forEach(function (fn) { fn(); }); }
    function set(value, persist) {
        value = value === 'classic' ? 'classic' : 'simple';
        root.setAttribute('data-appearance', value);
        if (persist !== false) write(key, value);
        refresh();
    }
    function read(name, fallback) { try { return deps.localStorage.getItem(name); } catch (_) { return fallback; } }
    function write(name, value) {
        try { deps.localStorage.setItem(name, value); delete unsaved[name]; }
        catch (_) { unsaved[name] = { baseline: read(name, null) }; }
    }
    function restored(name, current, external) {
        var saved = read(name, current), pending = unsaved[name];
        // A failed persistence must not undo this page's choice on BFCache return.
        // An observed external change wins; otherwise retain the unsaved choice.
        if (!external && pending && saved === pending.baseline) return current;
        delete unsaved[name]; return saved;
    }
    function syncAppearance(external) { set(restored(key, get(), external), false); }
    function syncTheme(external) {
        var theme = restored('papa-theme', root.getAttribute('data-theme'), external);
        if (typeof deps.setTheme === 'function') deps.setTheme(theme, false);
    }
    lifecycle.listen(deps.window,'storage', function (event) {
        try { if (event.storageArea !== deps.localStorage) return; } catch (_) { return; }
        if (event.key === key || event.key === null) syncAppearance(true);
        if (event.key === 'papa-theme' || event.key === null) syncTheme(true);
    });
    lifecycle.listen(deps.window,'pageshow', function () { syncAppearance(); syncTheme(); });
    syncAppearance();
    return {
        get: get, set: set, refresh: refresh,
        persistTheme: function (value) { write('papa-theme', value); },
        subscribe: function (fn) { listeners.add(fn); return function () { listeners.delete(fn); }; }
    };
})();

deps.Papa.settings('appearance', '显示与动效', function (el) {
    function control(id, title, options) {
        var label = deps.document.createElement('label'), select = deps.document.createElement('select');
        label.className = 'appearance-control'; select.id = id;
        label.append(deps.document.createTextNode(title + ' '));
        options.forEach(function (v) { var option = deps.document.createElement('option'); option.value = v[0]; option.textContent = v[1]; select.append(option); });
        label.append(select); el.append(label); return select;
    }
    var appearance = control('appearance-style', '界面风格', [['simple','简约'],['classic','经典']]);
    var theme = control('appearance-theme', '明暗模式', [['light','浅色'],['dark','深色']]);
    var select = control('appearance-motion', '动效偏好', [['system','跟随系统'],['reduced','减弱动效']]);
    var p = deps.document.createElement('p'); p.className = 'hint';
    p.textContent = '偏好只保存于当前浏览器。切换风格保留当前页面和草稿；减弱动效保留静态加载提示，数据立即显示真实值。';
    var motion = typeof deps.Oao !== 'undefined' && deps.Oao.motion;
    if (motion) select.value = motion.get();
    else { select.disabled = true; p.textContent += ' 当前 oao 未提供动效扩展；系统减弱动效设置仍然生效。'; }
    function changeAppearance() { MonitorAppearance.set(appearance.value); }
    function changeTheme() { deps.setTheme(theme.value); }
    function changeMotion() { if (motion) motion.set(select.value); }
    function sync() { appearance.value = MonitorAppearance.get(); theme.value = deps.getTheme(); }
    var unsubscribe = MonitorAppearance.subscribe(sync); sync();
    appearance.addEventListener('change', changeAppearance); theme.addEventListener('change', changeTheme); select.addEventListener('change', changeMotion);
    el.append(p);
    return function () {
        unsubscribe();
        appearance.removeEventListener('change', changeAppearance); theme.removeEventListener('change', changeTheme); select.removeEventListener('change', changeMotion);
    };
});



return Object.defineProperties({}, {
"MonitorAppearance":{enumerable:true,get:function(){return MonitorAppearance;},set:function(value){MonitorAppearance=value;}}
});
});
MonitorShell.install("shell/appearance");
