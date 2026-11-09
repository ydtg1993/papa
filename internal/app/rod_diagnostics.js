Papa.page('browser-diagnostics', function(root) {
    var disposed = false, busy = false;
    var intro = document.createElement('p');
    intro.textContent = '按当前配置启动独立临时浏览器，检查环境、临时目录、Chromium、Rod 连接及空白页面。最长约两分钟，结束后清理临时浏览器，报告同时写入系统日志。';
    var start = document.createElement('button'); start.className = 'btn'; start.textContent = '开始诊断';
    var status = document.createElement('p'); status.setAttribute('role', 'status');
    var report = document.createElement('textarea'); report.readOnly = true; report.style.width = '100%'; report.style.height = '50vh'; report.setAttribute('aria-label', '浏览器诊断报告');
    var copy = document.createElement('button'); copy.className = 'btn'; copy.textContent = '复制报告'; copy.disabled = true;
    copy.onclick = function() { copyText(report.value); };
    start.onclick = async function() {
        if (busy) return;
        busy = true; start.disabled = true; copy.disabled = true; report.value = ''; status.textContent = '正在诊断…';
        try {
            var response = await apiRequest('/api/browser-diagnostics', {method:'POST', rawHTTPError:true, current:function(){return !disposed;}});
            if (!response) throw new Error('诊断结果尚未确认，请查看系统日志');
            if (!response.ok) throw new Error(await response.text());
            var data = await response.json();
            if (disposed) return;
            status.textContent = data.ok ? '诊断通过' : '诊断发现问题';
            report.value = JSON.stringify(data, null, 2); copy.disabled = false;
        } catch (error) { if (!disposed) status.textContent = error.message; }
        finally { busy = false; if (!disposed) start.disabled = false; }
    };
    root.append(intro, start, status, report, copy);
    return {destroy:function(){disposed = true;}};
});
