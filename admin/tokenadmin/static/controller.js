/* tokenadmin/controller: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("tokenadmin/controller","tokens",["Dialog","Toast","apiFetch","apiPost","captureMonitorView","document","esc","saveKey","skeletonRows","startPolling","stopPolling","window"],function(deps,lifecycle){
'use strict';
var tokenRequestSeq = 0;
    /* ============ 访问令牌 ============ */
    /**
     * 这一页是后台自带的模块，不是 oao 表格页：表格组件的动作只回 {"status":"ok"}、不回数据，
     * 而「新增」必须把服务端生成的明文令牌交给操作人看一次（库里只存 sha256，过后无从显示）。
     */
    async function fetchTokens() {
        var el = deps.document.getElementById('token-table');
        if (!el) return;
        var seq = ++tokenRequestSeq, viewCurrent = deps.captureMonitorView();
        var current = function () { return seq === tokenRequestSeq && viewCurrent(); };
        el.innerHTML = '<table><tbody>' + deps.skeletonRows(7, 3) + '</tbody></table>';
        var resp = await deps.apiFetch('/api/tokens', { current: current });
        if (!current()) return;
        if (!resp) { el.innerHTML = '<div class="empty">读取失败，请稍后重试</div>'; return; }
        var data;
        try { data = await resp.json(); } catch (_) { if (current()) el.innerHTML = '<div class="empty">令牌响应格式无效，请刷新重试</div>'; return; }
        if (!current()) return;
        renderTokens(data.tokens || []);
    }

    function renderTokens(rows) {
        var el = deps.document.getElementById('token-table');
        if (!rows.length) {
            el.innerHTML = '<div class="empty">还没有任何访问令牌，管理接口默认拒绝访问。'
                + '请在服务器本机执行 papa token add --operator &lt;名字&gt; 创建凭据。</div>';
            return;
        }
        var html = '<table><thead><tr><th>ID</th><th>操作人</th><th>状态</th><th>备注</th>'
            + '<th>创建时间</th><th>更新时间</th><th>操作</th></tr></thead><tbody>';
        rows.forEach(function (t) {
            html += '<tr>'
                + '<td>' + t.id + '</td>'
                + '<td>' + deps.esc(t.operator) + '</td>'
                + '<td>' + (t.enabled
                    ? '<span class="badge ok">启用中</span>'
                    : '<span class="badge off">已停用</span>') + '</td>'
                + '<td>' + deps.esc(t.note || '') + '</td>'
                + '<td>' + fmtTime(t.created_at) + '</td>'
                + '<td>' + fmtTime(t.updated_at) + '</td>'
                + '<td><div class="row-actions">'
                + (t.enabled
                    ? '<button class="btn" data-token-off="' + t.id + '">停用</button>'
                    : '<button class="btn" data-token-on="' + t.id + '">启用</button>')
                + '<button class="btn danger" data-token-del="' + t.id + '">删除</button>'
                + '</div></td></tr>';
        });
        el.innerHTML = html + '</tbody></table>';

        el.querySelectorAll('[data-token-off]').forEach(function (b) {
            b.onclick = function () { setTokenEnabled(Number(b.dataset.tokenOff), false); };
        });
        el.querySelectorAll('[data-token-on]').forEach(function (b) {
            b.onclick = function () { setTokenEnabled(Number(b.dataset.tokenOn), true); };
        });
        el.querySelectorAll('[data-token-del]').forEach(function (b) {
            var update = deps.document.createElement('button'); update.className = 'btn'; update.textContent = '更新';
            b.parentNode.insertBefore(update, b);
            update.onclick = function () { rotateToken(Number(b.dataset.tokenDel), update); };
        });
        el.querySelectorAll('[data-token-del]').forEach(function (b) {
            b.onclick = function () { removeToken(Number(b.dataset.tokenDel)); };
        });
    }

    /** 后端给的是 RFC3339（带纳秒），浏览器 Date 只认到毫秒：多出来的小数位先截掉 */
    function fmtTime(s) {
        if (!s) return '';
        var d = new Date(String(s).replace(/(\.\d{3})\d+/, '$1'));
        return isNaN(d.getTime()) ? String(s) : d.toLocaleString();
    }

    /** 新增：表单 → 服务端生成 → 弹窗把明文显示一次 */
    async function openNewToken() {
        var current = deps.captureMonitorView();
        var p = deps.Dialog.open({
            title: '新增访问令牌',
            body: '<div class="field"><label for="tk-operator">操作人</label>'
                + '<input id="tk-operator" placeholder="谁用这把令牌，比如 张三" autocomplete="off"></div>'
                + '<div class="field"><label for="tk-note">备注</label>'
                + '<input id="tk-note" placeholder="可选，比如 运维机" autocomplete="off"></div>'
                + '<div class="hint">令牌由服务端生成，<b>只显示这一次</b>；库里只存 sha256，过后找不回来。</div>'
                + '<div class="errtext" id="tk-err"></div>',
            actions: [
                { label: '取消', value: null },
                {
                    label: '创建', value: 'ok',
                    onClick: function () {
                        if (!deps.document.getElementById('tk-operator').value.trim()) {
                            deps.document.getElementById('tk-err').textContent = '操作人不能为空';
                            deps.document.getElementById('tk-operator').focus();
                            return false; // 别关，让用户补上
                        }
                        return true;
                    }
                }
            ]
        });
        var operator = deps.document.getElementById('tk-operator');
        var note = deps.document.getElementById('tk-note');

        if (await p !== 'ok' || !current()) return;
        var data = await tokenWrite('/api/tokens', { operator: operator.value, note: note.value }, current);
        if (!data || !data.token || !current()) return;

        var token = data.token;
        await deps.Dialog.open({
            title: '令牌（只显示这一次）',
            body: '<p>给「' + deps.esc(operator.value.trim()) + '」的访问令牌：</p>'
                + '<div class="secret-box">' + deps.esc(token) + '</div>'
                + '<p class="hint">请立刻存好：库里只有 sha256，关掉这个窗口就再也看不到明文了。</p>',
            actions: [
                { label: '复制', value: 'copy', onClick: function () { copyText(token); return false; } },
                { label: '我已存好', value: null }
            ]
        });
        if (current()) fetchTokens();
    }

    async function setTokenEnabled(id, enabled) {
        var current = deps.captureMonitorView();
        if (!enabled) {
            var ok = await deps.Dialog.confirm({
                title: '停用访问令牌',
                body: '<p>停用后这把令牌立刻失效（可以再启用）。</p>'
                    + '<p>如果它是最后一把启用中的令牌，后台将对所有人拒绝，'
                    + '那种情况只能用 <code>papa token add</code> 救回来。</p>',
                danger: true, okLabel: '停用'
            });
            if (!ok || !current()) return;
        }
        if (!await tokenWrite('/api/tokens/enabled', { id: id, enabled: enabled }, current) || !current()) return;
        deps.Toast.ok(enabled ? '已启用' : '已停用');
        fetchTokens();
    }

    async function removeToken(id) {
        var current = deps.captureMonitorView();
        var ok = await deps.Dialog.confirm({
            title: '删除访问令牌',
            body: '<p>删除后无法恢复（这条令牌与它的哈希一起删掉）。</p><p>只是暂时不用的话，改用「停用」。</p>',
            danger: true, okLabel: '删除'
        });
        if (!ok || !current()) return;
        if (!await tokenWrite('/api/tokens/remove', { id: id }, current) || !current()) return;
        deps.Toast.ok('已删除');
        fetchTokens();
    }

    /** 令牌写入复用正文超时、认证代次和服务端错误提示。 */
    async function tokenWrite(url, body, current) {
        current = current || deps.captureMonitorView();
        if (!current()) return null;
        var resp = await deps.apiPost(url, body);
        if (!resp || !current()) return null;
        try { var data = await resp.json(); return current() ? data : null; }
        catch (e) { if (current()) deps.Toast.err('响应读取失败，操作结果尚未确认，请刷新后查看'); return null; }
    }

    /** 复制到剪贴板；明文令牌是长串随机字符，靠手抄容易错一位 */
    function copyText(text) {
        if (navigator.clipboard && deps.window.isSecureContext) {
            navigator.clipboard.writeText(text).then(
                function () { deps.Toast.ok('已复制到剪贴板'); },
                function () { deps.Toast.err('复制失败，请手动选中'); });
            return;
        }
        // 非安全上下文（http + 局域网 IP）拿不到 navigator.clipboard，退回选中 + execCommand
        var ta = deps.document.createElement('textarea');
        ta.value = text;
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        deps.document.body.appendChild(ta);
        ta.select();
        var ok = false;
        try { ok = deps.document.execCommand('copy'); } catch (e) { ok = false; }
        deps.document.body.removeChild(ta);
        if (ok) deps.Toast.ok('已复制到剪贴板'); else deps.Toast.err('复制失败，请手动选中');
    }
    // 关停是高危操作：要人在场，所以让操作人把自己的访问令牌再输一遍。
    // 服务端会拿它去查令牌表（`/api/settings/shutdown` 的 body 里带 token），输错返回 403。
    // 用 Dialog.open 而不是 confirm —— 需要一个输入框，并且要在点「退出」时校验非空。
async function rotateToken(id, button) {
    if (button.disabled) return;
    button.disabled = true;
    var current = deps.captureMonitorView();
    try {
        if (!await deps.Dialog.confirm({title:'更新访问令牌',body:'<p>更新后旧令牌立即失效。新令牌只显示一次，请及时保存。响应丢失时可用本机 papa token add 恢复访问。</p>',okLabel:'更新'}) || !current()) return;
        deps.stopPolling();
        var data = await tokenWrite('/api/tokens/rotate', {id:id}, current);
        if (!data || !data.token || !current()) return;
        if (data.current) deps.saveKey(data.token);
        await deps.Dialog.open({title:'新令牌（只显示一次）',body:'<div class="secret-box">'+deps.esc(data.token)+'</div>',actions:[
            {label:'复制',onClick:function(){copyText(data.token);return false;}},{label:'已保存',value:true}
        ]});
        fetchTokens();
    } finally { button.disabled = false; deps.startPolling(); }
}


function reset(){tokenRequestSeq++;var el=deps.document.getElementById('token-table');if(el)el.textContent='';}

function unmount(){tokenRequestSeq++;}



return Object.defineProperties({}, {
"fetchTokens":{enumerable:true,get:function(){return fetchTokens;},set:function(value){fetchTokens=value;}},
"renderTokens":{enumerable:true,get:function(){return renderTokens;},set:function(value){renderTokens=value;}},
"fmtTime":{enumerable:true,get:function(){return fmtTime;},set:function(value){fmtTime=value;}},
"openNewToken":{enumerable:true,get:function(){return openNewToken;},set:function(value){openNewToken=value;}},
"setTokenEnabled":{enumerable:true,get:function(){return setTokenEnabled;},set:function(value){setTokenEnabled=value;}},
"removeToken":{enumerable:true,get:function(){return removeToken;},set:function(value){removeToken=value;}},
"tokenWrite":{enumerable:true,get:function(){return tokenWrite;},set:function(value){tokenWrite=value;}},
"copyText":{enumerable:true,get:function(){return copyText;},set:function(value){copyText=value;}},
"rotateToken":{enumerable:true,get:function(){return rotateToken;},set:function(value){rotateToken=value;}},
"reset":{enumerable:true,get:function(){return reset;},set:function(value){reset=value;}},
"unmount":{enumerable:true,get:function(){return unmount;},set:function(value){unmount=value;}}
});
});
MonitorShell.install("tokenadmin/controller");
