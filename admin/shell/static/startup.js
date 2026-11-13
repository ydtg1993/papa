/* shell/startup: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/startup",null,["Oao","authRequired","closeBell","document","handleTableUnauthorized","hideLogin","loadKey","setNavigationOpen","showLogin","startPolling","submitKey","syncMonitorNavigation"],function(deps,lifecycle){
'use strict';
    /* ============ 启动 ============ */
    deps.document.getElementById('key').addEventListener('keydown', function (e) { if (e.key === 'Enter') deps.submitKey(); });

    // 铃铛：点面板外部、按 Esc 关掉它（注册一次就够，开合状态在 toggleBell / closeBell 里）。
    // 点铃铛自己与面板里的按钮都算"内部"—— 否则点「恢复」时面板会先被关掉（虽然请求照发，
    // 但列表会在下一轮刷新前空一下）。
    lifecycle.listen(deps.document,'click', function (ev) {
        var wrap = deps.document.getElementById('bell-wrap');
        if (wrap && ev.target && wrap.contains(ev.target)) return;
        deps.closeBell();
    });
    lifecycle.listen(deps.document,'keydown', function (ev) {
        if (ev.key !== 'Escape') return;
        deps.closeBell();
        var toggle = deps.document.getElementById('nav-toggle');
        if (toggle && toggle.getAttribute('aria-expanded') === 'true') { deps.setNavigationOpen(false); toggle.focus(); }
    });

    // 表格动作的防重复提交由 oao 组件自己做（runAction 里的 inFlightActions，
    // key 用「表 + 动作 + 主键」）—— v1.2.3 及以前没有，v1.2.4 起有。
    // 宿主不用再包一层；服务端的条件更新始终是最终防线。

    // oao 表格组件：复用同一套访问密钥；接口前缀与静态资源由 papa 挂载
    deps.Oao.init({
        base: '/api/oao',
        headers: function () {
            var key = deps.loadKey();
            return key ? { 'Authorization': 'Bearer ' + key } : {};
        },
        // 密钥失效时和其它接口保持一致：清掉旧密钥、弹登录，而不是只在表格区留一行错误
        // （返回 false 表示没换成新凭据，组件就把 401 如实抛出来）
        onUnauthorized: deps.handleTableUnauthorized
    });

    if (typeof deps.syncMonitorNavigation === 'function') deps.syncMonitorNavigation();

    // 有本地凭据或宿主使用自定义登录界面时开始加载；默认无凭据时停在登录框，
    // 等登录成功后再开始轮询/渲染菜单（避免未登录状态下每 3 秒刷一次 401）。
    if (deps.authRequired() && !deps.loadKey()) {
        deps.showLogin(false);
    } else {
        deps.hideLogin();
        deps.startPolling();
    }




return Object.defineProperties({}, {

});
});
MonitorShell.install("shell/startup");
