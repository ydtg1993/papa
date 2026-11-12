/* shell/theme: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/theme",null,["MonitorAppearance","document","localStorage"],function(deps,lifecycle){
'use strict';
/**
 * Papa Monitor — 前台逻辑
 * 由 mo.js 重构而来：保留主题切换，加入监控后台的取数/渲染/设置逻辑；
 * 表格页由 oao 组件（github.com/ydtg1993/oao）渲染，本文件只负责菜单与装配。
 * 顶层函数/变量需保持全局，供 template.html 的内联 onclick 调用。
 */
'use strict';

/* ============ 主题切换 ============ */
    var themeKey = 'papa-theme';
    function getTheme() { return deps.document.documentElement.getAttribute('data-theme') === 'dark' ? 'dark' : 'light'; }
    function setTheme(t, persist) {
        t = t === 'dark' ? 'dark' : 'light';
        deps.document.documentElement.setAttribute('data-theme', t);
        if (persist !== false) {
            if (typeof deps.MonitorAppearance !== 'undefined') deps.MonitorAppearance.persistTheme(t);
            else { try { deps.localStorage.setItem(themeKey, t); } catch (_) { /* Display remains usable without storage. */ } }
        }
        var btn = deps.document.getElementById('theme-toggle');
        if (btn) btn.textContent = t === 'dark' ? '☀ 浅色' : '☾ 深色';
        if (typeof deps.MonitorAppearance !== 'undefined') deps.MonitorAppearance.refresh();
    }
    setTheme(getTheme(), false);
    var themeBtn = deps.document.getElementById('theme-toggle');
    if (themeBtn) themeBtn.addEventListener('click', function () {
        setTheme(getTheme() === 'dark' ? 'light' : 'dark');
    });




return Object.defineProperties({}, {
"getTheme":{enumerable:true,get:function(){return getTheme;},set:function(value){getTheme=value;}},
"setTheme":{enumerable:true,get:function(){return setTheme;},set:function(value){setTheme=value;}}
});
});
MonitorShell.install("shell/theme");
