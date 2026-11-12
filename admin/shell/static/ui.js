/* shell/ui: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("shell/ui",null,["document"],function(deps,lifecycle){
'use strict';
    /* ============ UI 基础组件（阶段 0） ============ */

    /** Toast 右下角浮层反馈；容器挂在 body 上，不受模块重绘影响 */
    var Toast = (function () {
        var box = null;
        function ensure() {
            if (!box) {
                box = deps.document.createElement('div');
                box.className = 'toasts';
                deps.document.body.appendChild(box);
            }
            return box;
        }
        return {
            show: function (msg, type, ms) {
                var el = deps.document.createElement('div');
                el.className = 'toast ' + (type || 'info');
                el.textContent = msg;            // textContent：消息内容不解析 HTML
                ensure().appendChild(el);
                setTimeout(function () { el.remove(); }, ms || 3200);
            },
            ok: function (msg, ms) { this.show(msg, 'ok', ms); },
            err: function (msg, ms) { this.show(msg, 'err', ms); },
            info: function (msg, ms) { this.show(msg, 'info', ms); }
        };
    })();

    /**
     * Dialog 模态弹窗。open 返回 Promise：
     *   - 点击某个按钮 -> resolve(该按钮的 value)
     *   - 点遮罩 / 按 Esc / 点取消 -> resolve(null)
     * 用法：Dialog.alert({...})、Dialog.confirm({...}) 是常用封装。
     */
    var Dialog = (function () {
        var mask = null, elTitle = null, elBody = null, elActions = null;
        var settle = null;   // 当前 Promise 的 resolve
        var returnFocus = null;

        function ensure() {
            if (mask) return;
            mask = deps.document.createElement('div');
            mask.className = 'dlg-mask';
            mask.innerHTML = '<div class="dlg" role="dialog" aria-modal="true" aria-labelledby="papa-dialog-title" tabindex="-1">'
                + '<h3 id="papa-dialog-title"></h3><div class="dlg-body"></div><div class="dlg-actions"></div></div>';
            mask.addEventListener('mousedown', function (e) { if (e.target === mask) close(null); });
            deps.document.body.appendChild(mask);
            elTitle = mask.querySelector('h3');
            elBody = mask.querySelector('.dlg-body');
            elActions = mask.querySelector('.dlg-actions');
            lifecycle.listen(deps.document,'keydown', function (e) {
                if (!mask.classList.contains('open')) return;
                if (e.key === 'Escape') { e.preventDefault(); close(null); }
                if (e.key === 'Tab') {
                    var targets = Array.from(mask.querySelectorAll('button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')).filter(function (el) { return el.offsetParent !== null; });
                    var first = targets[0], last = targets[targets.length - 1];
                    if (!first) { e.preventDefault(); mask.querySelector('.dlg').focus(); }
                    else if (!mask.contains(deps.document.activeElement) || (e.shiftKey && deps.document.activeElement === first) || (!e.shiftKey && deps.document.activeElement === last)) {
                        e.preventDefault(); (e.shiftKey ? last : first).focus();
                    }
                }
            });
        }

        function close(value) {
            if (!mask || !mask.classList.contains('open')) return;
            mask.classList.remove('open');
            var done = settle;
            settle = null;
            if (returnFocus && returnFocus.isConnected) returnFocus.focus();
            // The opener may have been replaced by a render. A hidden dialog
            // must not retain focus while the caller restores its new button.
            if (mask.contains(deps.document.activeElement)) deps.document.activeElement.blur();
            returnFocus = null;
            if (done) done(value);
        }

        function open(opts) {
            ensure();
            // A late Trace can replace a pending confirmation. Always settle the
            // displaced dialog so callers release their operation locks.
            close(null);
            returnFocus = deps.document.activeElement;
            opts = opts || {};
            mask.querySelector('.dlg').classList.toggle('danger', !!opts.danger);
            elTitle.textContent = opts.title || '提示';
            elBody.innerHTML = opts.body || '';        // 调用方负责转义
            elActions.innerHTML = '';
            var actions = opts.actions || [{ label: '知道了', value: 'ok' }];
            actions.forEach(function (a) {
                var b = deps.document.createElement('button');
                b.className = 'btn' + (a.tone === 'danger' ? ' danger' : '');
                b.textContent = a.label;
                b.onclick = function () {
                    // 表单类弹窗用 onClick 先校验：返回 false 表示"先别关"（关闭会清掉表单）
                    if (a.onClick && a.onClick() === false) return;
                    close(a.value);
                };
                elActions.appendChild(b);
            });
            mask.classList.add('open');
            var first = elActions.querySelector('.btn');
            if (first) first.focus();
            return new Promise(function (resolve) { settle = resolve; });
        }

        return {
            open: open,
            /** alert 只有一个「知道了」按钮，resolve(true) */
            alert: function (opts) {
                var actions = [{ label: (opts && opts.okLabel) || '知道了', value: true }];
                return open(Object.assign({}, opts, { actions: actions }));
            },
            /** confirm 确认/取消，resolve 布尔 */
            confirm: function (opts) {
                opts = opts || {};
                return open({
                    title: opts.title || '请确认',
                    body: opts.body || '',
                    danger: opts.danger,
                    actions: [
                        { label: opts.cancelLabel || '取消', value: false },
                        { label: opts.okLabel || '确定', value: true, tone: opts.danger ? 'danger' : '' }
                    ]
                }).then(function (v) { return v === true; });
            },
            close: function () { close(null); },
            reset: function () {
                close(null);
                if (!mask) return;
                elTitle.textContent = ''; elBody.innerHTML = ''; elActions.innerHTML = '';
            }
        };
    })();

    /** skeleton 生成 n 行占位，用于表格加载态 */
    function skeletonRows(cols, n) {
        var html = '';
        for (var i = 0; i < n; i++) {
            html += '<tr class="skel-row">';
            for (var c = 0; c < cols; c++) html += '<td><span class="skel"></span></td>';
            html += '</tr>';
        }
        return html;
    }


    /** showSkeleton 表格加载占位；cols 不传则沿用当前表头列数 */
    function showSkeleton(id, cols) {
        var el = deps.document.getElementById(id);
        if (!el) return;
        var n = cols || el.querySelectorAll('thead th').length || 6;
        el.innerHTML = '<table><tbody>' + skeletonRows(n, 5) + '</tbody></table>';
    }
    function esc(s) {
        return String(s).replace(/[&<>"']/g, function (c) {
            return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
        });
    }
    /** tipAttr 生成 data-tip 属性（内容已转义），用于超长内容悬停查看全文 */
    function tipAttr(text) {
        var s = (text === null || text === undefined) ? '' : String(text);
        return s ? ' data-tip="' + esc(s) + '"' : '';
    }
    /* ============ 格式化 ============ */
    function fmtDuration(ns) {
        if (ns == null || isNaN(ns)) return '-';
        if (ns < 1e6) return (ns / 1e3).toFixed(2) + 'µs';
        if (ns < 1e9) return (ns / 1e6).toFixed(2) + 'ms';
        return (ns / 1e9).toFixed(2) + 's';
    }
    function fmtBytes(b) {
        if (b == null) return '-';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = 0, v = b;
        while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
        return v.toFixed(i === 0 ? 0 : 1) + ' ' + units[i];
    }
    function pct(p) { return p == null ? 0 : Math.min(100, Math.max(0, p)); }
    function bar(cls, p) { return '<div class="bar ' + cls + '"><span style="width:' + p + '%"></span></div>'; }




return Object.defineProperties({}, {
"Toast":{enumerable:true,get:function(){return Toast;},set:function(value){Toast=value;}},
"Dialog":{enumerable:true,get:function(){return Dialog;},set:function(value){Dialog=value;}},
"skeletonRows":{enumerable:true,get:function(){return skeletonRows;},set:function(value){skeletonRows=value;}},
"showSkeleton":{enumerable:true,get:function(){return showSkeleton;},set:function(value){showSkeleton=value;}},
"esc":{enumerable:true,get:function(){return esc;},set:function(value){esc=value;}},
"tipAttr":{enumerable:true,get:function(){return tipAttr;},set:function(value){tipAttr=value;}},
"fmtDuration":{enumerable:true,get:function(){return fmtDuration;},set:function(value){fmtDuration=value;}},
"fmtBytes":{enumerable:true,get:function(){return fmtBytes;},set:function(value){fmtBytes=value;}},
"pct":{enumerable:true,get:function(){return pct;},set:function(value){pct=value;}},
"bar":{enumerable:true,get:function(){return bar;},set:function(value){bar=value;}}
});
});
MonitorShell.install("shell/ui");
