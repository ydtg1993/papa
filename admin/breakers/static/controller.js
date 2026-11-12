/* breakers/controller: private state, explicit dependencies, per-instance lifecycle. */
MonitorShell.define("breakers/controller","dashboard",["Dialog","Toast","apiFetch","apiPost","captureMonitorView","copyText","document","downloadFile","esc","fetchData","fmtSpan","siteLabel","timeValid"],function(deps,lifecycle){
'use strict';
    /* ---- 暂停与告警：右上角铃铛 + 下拉面板（多站时逐 scope 一行） ---- */
    // 为什么不是红色横幅：声明了 `SiteSpec.StartPaused` 的站一加载就停着，横幅会变成常驻的一大块
    //（把正文挤下去），而它平时什么也不必说。铃铛把"有东西停着"压成一个徽标数字：
    //   - 数字 = 被闸住的 scope 数（含声明期的 StartPaused）；
    //   - 颜色 = 语义：里面有熔断触发就是红的，只有手动/声明期就用琥珀 —— 一眼看出"是不是出事了"。
    // 点开才列出细节（站点 / 阶段 / 原因 / 暂停于 + 逐站「恢复」）；再点一次、点面板外部、按 Esc 关闭。
    // 状态跟着 /api/monitor 每轮刷新一起来（不额外发请求），所以别的标签页 / 别的操作人触发的
    // 熔断，这边几秒内也能看到。
    //
    // 面板**不跟站点 Tab 过滤**（告警不该被切走的 tab 藏起来）；跟 tab 走的是仪表盘上那张
    // 「站点控制」卡 —— 那张是控制视角，这张是告警视角。
    var bellSeen = null; // 上一轮见过的熔断 scope；null = 还没画过（首帧不脉冲，免得一进页面就抖）
    function renderBell(list) {
        var btn = deps.document.getElementById('bell');
        var badge = deps.document.getElementById('bell-badge');
        var panel = deps.document.getElementById('bell-panel');
        if (!btn || !badge || !panel) return;
        var paused = (list || []).filter(function (b) { return b && b.paused; });
        var tripped = paused.filter(function (b) { return b.source === 'breaker'; });

        // 徽标：数字 = 被闸住的 scope 数，颜色分语义；没有就整个藏起来（空徽标比没徽标更吵）
        badge.hidden = !paused.length;
        badge.textContent = paused.length;
        badge.className = 'bell-badge' + (paused.length ? (tripped.length ? ' breaker' : ' manual') : '');

        // 脉冲只给**新出现的熔断**：页面开着时出事要看得见；而 StartPaused 那种"一加载就停着"
        // 不该被当成新告警（首帧也不算 —— 否则每次刷新页面都抖一下）。
        var now = tripped.map(scopeKey);
        if (bellSeen !== null && now.some(function (k) { return bellSeen.indexOf(k) < 0; })) pulseBell(btn);
        bellSeen = now;

        var html = paused.map(bellRow).join('');
        if (paused.length > 1) {
            html += '<div class="bell-all"><span class="bell-meta">共 ' + paused.length +
                ' 个 scope 被闸住</span>' +
                '<button class="btn" onclick="resumeCrawling()">全部重试恢复</button></div>';
        }
        if (!html) {
            html = '<div class="bell-meta">当前没有被闸住的站点</div>';
        }
        panel.innerHTML = html; // 每一段都过了 esc()
    }

    // scopeKey 一个 scope 的稳定标识：站点 + 阶段（阶段级暂停的 scope 带 stage，整站的不带）。
    // 用 \u0000 拼是为了两个字段怎么取值都不会拼出同一个键。
    function scopeKey(b) { return (b.site || '') + '\u0000' + (b.stage || ''); }

    // pulseBell 脉冲两下。先把上一次的类摘掉再加回去 —— 中间必须读一次布局属性强制重排，
    // 否则"摘掉"与"加回"会被浏览器合并成一次样式变更，动画不会重新开始（第二次告警就不抖了）。
    function pulseBell(btn) {
        btn.classList.remove('pulse');
        void btn.offsetWidth;
        btn.classList.add('pulse');
    }

    // bellRow 面板里的一行：**两行式**（头部一行 + 一条等宽小字的 meta）。
    // 面板只有五百来像素宽，把"整句散文 + 按钮"塞进一行会折成三四行、按钮悬在中间；
    // 拆开之后第一行永远是"谁 + 结论 + 动作"，细节退到第二行。
    function bellRow(b) {
        var at = b.paused_at ? new Date(b.paused_at) : null;
        var when = (at && deps.timeValid(at)) ? at.toLocaleString() : '';
        // **手动暂停与熔断触发分开说**：一律按熔断文案渲染就会读出"阈值 0 / 窗口内 0 次失败"
        // 这种幻觉（熔断没启用时阈值就是 0），也是一次手动暂停被说成"熔断"。
        //
        // 判据只看 `source`。**别回去用"enabled && stage"猜**：阶段级的手动暂停也带 stage
        //（那是它的 scope），在熔断启用着的站上会被猜成熔断 —— 这正是加 source 字段的原因。
        var tripped = b.source === 'breaker';
        var head = tripped ? '抓取已暂停（熔断）' : ('抓取已暂停（' + deps.esc(b.reason || '手动') + '）');
        // meta 的几段：有哪段写哪段（整站暂停没有阶段；熔断才有窗口与失败数）
        var meta = [];
        if (b.stage) meta.push('阶段 ' + deps.esc(b.stage));
        if (tripped) {
            meta.push(deps.esc(deps.fmtSpan(b.window / 1e9)) + ' 窗口内 ' + (b.failures || 0) +
                ' 次终态失败，阈值 ' + (b.threshold || 0));
        }
        if (when) meta.push('暂停于 ' + deps.esc(when));
        return '<div class="bell-item">' +
            '<div class="bell-head">' +
            '<span class="bell-where">站点 ' + deps.esc(b.site || '') + '</span>' +
            '<span class="bell-head-text">' + head + '</span>' +
            (tripped ? '<button class="btn" data-site="' + deps.esc(b.site || '') + '" onclick="openBreakerAnalysis(this.dataset.site)">分析原因</button>' : '') +
            '<button class="btn" data-scope="' + deps.esc(b.site || '') +
            '" onclick="resumeCrawling(this.dataset.scope)">重试恢复</button>' +
            '</div>' +
            (meta.length ? '<div class="bell-meta">' + meta.join('｜') + '</div>' : '') +
            '</div>';
    }

    // 铃铛的开合：点铃铛切换；点面板外部、按 Esc 关闭（后两个监听在文件末尾注册一次）。
    function toggleBell(ev) {
        if (ev) ev.stopPropagation(); // 不拦住的话，"点面板外部就关"会当场把它关回去
        var panel = deps.document.getElementById('bell-panel');
        var btn = deps.document.getElementById('bell');
        if (!panel || !btn) return;
        panel.hidden = !panel.hidden;
        btn.setAttribute('aria-expanded', panel.hidden ? 'false' : 'true');
    }

    function closeBell() {
        var panel = deps.document.getElementById('bell-panel');
        var btn = deps.document.getElementById('bell');
        if (!panel || panel.hidden) return;
        panel.hidden = true;
        if (btn) btn.setAttribute('aria-expanded', 'false');
    }
    // site 省略 = 全部放行（多站时后台横幅上的「全部恢复」）；stage 省略 = 那一站的整站那一层。
    // `stage` 这一档框架自带的界面**不用**（卡片只做到站点级），它是留给业务自己接的：
    // 注进来的页/脚本调 `pauseCrawling('example', 'catalog')` 就能只掐那一个阶段。
    async function resumeCrawling(site, stage) {
        // 有 stage 没 site 是调用错误：老代码会把它当成「全部放行」（body 发成 {}），
        // 服务端把"无 scope"解释成全部站点、逐站清暂停 —— 一个"恢复某阶段"的手滑变成"恢复所有站"。
        // 服务端已拒绝这类参数（400），这里先拦住并说清楚，别让业务脚本踩成静默的越权放行。
        if (stage && !site) {
            deps.Toast.err('恢复某个阶段需要同时给出站点（scope）');
            return;
        }
        var body = site ? { scope: site } : {};
        if (site && stage) body.stage = stage;
        var resp = await deps.apiPost('/api/breaker/resume', body);
        if (!resp || !resp.ok) return;
        var data = null;
        try { data = await resp.json(); } catch (e) { /* 拿不到 body 不影响放行结果 */ }
        // resumed=false 说明本来就没闸住（比如另一个人先点了）—— 说清楚，别报个含糊的成功
        if (data && data.resumed === false) deps.Toast.info('未变更：当前无需恢复，或探针尚未退出');
        else deps.Toast.ok(stage ? ('已解除阶段人工暂停 ' + site + '/' + stage) : '已请求重试恢复；自动保护需通过全阶段验证后解除');
        deps.fetchData();
    }

    async function forceResumeCrawling(site) {
        if (!site) return;
        if (!await deps.Dialog.confirm({title:'强制解除保护',body:'<p>将跳过整站恢复验证。失败计数保留，再次失败可能立即暂停。</p>',okLabel:'强制解除'})) return;
        var resp = await deps.apiPost('/api/breaker/force-resume', {scope:site});
        if (!resp || !resp.ok) return;
        var data = await resp.json();
        if (data.resumed) deps.Toast.ok('已强制解除保护；未确认站点健康');
        else deps.Toast.info('当前没有需要解除的保护');
        deps.fetchData();
    }

    /* ---- 站点控制：一站一行（站点名 + 未暂停/已暂停 + 暂停/恢复整站） ---- */
    // 为什么要有：**站点那一层**是唯一不需要站点知识的粒度（"这一站先别抓"），也是后台**唯一**
    // 主动暂停的入口 —— 熔断横幅只在已经闸住时才出现、只有「恢复」。声明了 `StartPaused` 的站
    // 启动就停在这一档上，操作人在这里点开（见 docs/MONITOR.md §10）。
    //
    // 阶段那一层（只停某一站的某一个阶段）是**接口**：端点带 stage、引擎侧 PauseStage / ResumeStage
    // 都在，框架不替业务决定界面长什么样 —— 注进来的页/脚本调 `pauseCrawling('example', 'catalog')`
    // 就行。这里刻意不做成每个阶段一行：那是站点知识，框架没有。
    //
    // 数据：sites（跟着站点 Tab 过滤，概览 = 全部站点）+ breakers（与横幅同一份暂停状态，
    // 不额外发请求）。两个回调都没配时整块隐藏，不占位。
    function renderSiteControl(sites, breakers) {
        var sec = deps.document.getElementById('dash-site-ctl-sec');
        var el = deps.document.getElementById('dash-site-ctl');
        if (!sec || !el) return;
        var breakerOf = {};
        (breakers || []).forEach(function (b) { if (b) breakerOf[b.site] = b; });

        // sitePaused：这一站**整站**那一层停没停。
        // 注意 breakers 里这一站 paused=true 也可能是"某个阶段停着"（阶段级暂停也算这一站有暂停），
        // 所以要靠 source + stage 区分：**手动 + 带 stage = 阶段级**；熔断触发永远是整站那一层
        //（它的 stage 是"当时哪个阶段在失败"，不是停的范围）。
        function sitePaused(site) {
            var b = breakerOf[site];
            if (!b || !b.paused) return false;
            return !(b.source === 'manual' && b.stage);
        }

        // 站点清单由调用方给（以 sites 为准）：**每个站一行**，哪怕它没有入口阶段 ——
        // 这个「暂停/恢复」是整站的控制，不该因为"这个站没有入口"就消失。
        var list = (sites || []).slice();
        if (!list.length) { sec.hidden = true; el.innerHTML = ''; return; }

        el.innerHTML = list.map(function (site) {
            var paused = sitePaused(site);
            // 运行期值一律走 data-*（内联属性里只有固定代码）—— 与熔断那个「恢复」按钮同一套写法
            var toggle = '<button class="btn' + (paused ? '' : ' danger') + '" data-site="' + deps.esc(site) + '" ' +
                'onclick="' + (paused ? 'resumeCrawling' : 'pauseCrawling') + '(this.dataset.site)">' +
                (paused ? '重试恢复' : '暂停整站') + '</button>';
            return '<div class="card"><div class="breaker-row">' +
                '<span class="breaker-text"><b>' + deps.esc(deps.siteLabel(site)) + '</b>' +
                (paused ? '｜<b>已暂停</b>' : '｜未暂停') + '</span>' +
                '<button class="btn" data-site="' + deps.esc(site) + '" onclick="openBreakerAnalysis(this.dataset.site)">熔断分析</button>' + toggle + ((breakerOf[site] && breakerOf[site].incident_id) ? '<button class="btn danger" data-site="'+deps.esc(site)+'" onclick="forceResumeCrawling(this.dataset.site)">强制解除保护</button>' : '') + '</div></div>';
        }).join('');
        sec.hidden = false;
    }

    var breakerAnalysisSeq = 0;
    async function openBreakerAnalysis(site) {
        var seq = ++breakerAnalysisSeq, viewCurrent = deps.captureMonitorView(), selected = null, refreshTimer = null;
        var dialog = deps.Dialog.open({title: site + ' · 熔断分析',
            body: '<p class="hint" id="breaker-analysis-status" role="status">正在读取熔断现场…</p>' +
                '<label>熔断记录 <select id="breaker-analysis-select" aria-label="熔断记录"></select></label>' +
                '<div id="breaker-analysis-body"></div>',
            actions: [
                {label:'复制报告',onClick:function(){if(selected && current())deps.copyText(selected.text);return false;}},
                {label:'下载文本',onClick:function(){download('text');return false;}},
                {label:'下载 JSON',onClick:function(){download('json');return false;}},
                {label:'关闭',value:true}
            ]});
        var status = deps.document.getElementById('breaker-analysis-status'), select = deps.document.getElementById('breaker-analysis-select'), body = deps.document.getElementById('breaker-analysis-body');
        var current = function(){return seq === breakerAnalysisSeq && viewCurrent() && body.isConnected;};
        dialog.then(function(){clearTimeout(refreshTimer);if(seq===breakerAnalysisSeq)breakerAnalysisSeq++;});
        async function download(format) {
            if (!selected || !current()) return;
            await deps.downloadFile('/api/breaker/analysis?scope='+encodeURIComponent(site)+'&id='+encodeURIComponent(selected.id)+'&format='+format,current);
        }
        function render(report) {
            if (!current()) return;
            selected = report;
            var rows = report.groups.map(function(g){return '<tr><td>'+deps.esc(g.stage)+'</td><td>'+deps.esc(g.category)+'</td><td>'+g.count+'</td><td>'+g.percent.toFixed(1)+'%</td><td>'+g.tasks+'</td></tr>';}).join('');
            var lifecycle = report.lifecycle;
            var stateNames = {interrupted:'验证已中断',pending:'待验证',probing:'试探中',passed:'验证通过',failed:'验证失败',missing:'缺少探针',observing:'受限观察',ramping:'整站限量验证',cooldown:'整站暂停冷却',manual:'暂停，等待定时复查或人工处理',closed:'人工结束事件',recovered:'整站已恢复'};
            var actionNames = {failure_pending:'真实失败待落库确认',failure_settlement_restart:'重启接续失败结算',failure_settlement_retry:'重试失败结算',failure_settlement_wait:'落库未确认，继续等待',failure_settled:'失败已确认并计数',sample_late_result:'记录迟到任务终态',recovery_upgraded:'升级恢复控制规则',sample_selected:'选中串行验证样本',sample_timeout:'验证样本超时',sample_exited:'样本执行已退出',sample_interrupted:'重启中断样本验证',sample_superseded:'结束已被完整证明替代的样本等待',stage_probe_timeout:'只读验证等待超时',hourly_recheck:'每小时复查',hourly_recheck_skipped:'本次复查未启动',sample_retry:'重试指定验证任务',sample_retry_unavailable:'验证任务无法重试',process_stopped:'进程正常停止',process_stop_incomplete:'进程未完全排空',process_restart:'进程重新启动',process_interrupted:'发现上次运行中断',previous_stage_snapshot:'保留上次阶段验证',history_start:'开始追加历史',history_capacity:'历史写入积压',manual_retry:'人工重试恢复',stage_probe_interrupted:'阶段验证中断',stage_probe_started:'开始阶段试探',stage_probe_passed:'阶段试探通过',stage_probe_failed:'阶段试探失败',stage_probe_missing:'阶段探针缺失',stage_task_success:'真实任务验证成功',task_failed:'真实任务失败',waiting_inflight:'等待在途任务结束',retry_after_wait:'等待服务端允许重试',manual_control:'人工操作',manual_pause:'人工暂停',manual_resume:'人工解除暂停'};
            var stageRows = lifecycle && (lifecycle.checks || []).map(function(c){return '<tr><td>'+deps.esc(c.stage)+'</td><td>'+deps.esc(c.state === 'passed' ? '探针/任务检查已完成' : (stateNames[c.state] || c.state))+'</td><td>'+deps.esc(c.verification === 'readonly' ? '完整只读验证' : '真实任务验证')+'<br>'+deps.esc(c.coverage || '未提供覆盖范围')+'</td><td>'+deps.esc(c.verification_note || '未验证：缺少验证结论')+'<br>'+deps.esc((c.sample_ids || []).join('，'))+'</td><td>'+Number(c.round)+'</td><td>'+Number(c.probe_successes || 0)+' / '+Number(c.task_successes || 0)+'</td><td>'+deps.esc(c.detail || '')+'</td></tr>';}).join('');
            var timelineRows = lifecycle && (lifecycle.timeline || []).map(function(t){return '<li><b>'+deps.esc(t.at)+'</b> · '+deps.esc(t.stage || '整站')+' · 第 '+Number(t.round)+' 轮 · '+deps.esc(actionNames[t.action] || stateNames[t.action] || t.action)+'<br>'+deps.esc(t.reason || '')+(t.check ? '<br>当时验证：'+deps.esc(t.check.verification)+'；覆盖：'+deps.esc(t.check.coverage)+'；样本：'+deps.esc((t.check.sample_ids || []).join('，'))+'；'+deps.esc(t.check.verification_note) : '')+'<br>窗口失败 / 成功：'+Number(t.failures)+' / '+Number(t.successes)+(t.next_attempt_at && !t.next_attempt_at.startsWith('0001-')?'；下次计划：'+deps.esc(t.next_attempt_at):'')+'</li>';}).join('');
            body.innerHTML = (lifecycle ? '<p><b>事件状态：</b>'+deps.esc(stateNames[lifecycle.state] || lifecycle.state)+' · '+deps.esc(lifecycle.stage || '整站')+'</p>'+
                (lifecycle.recheck_at && !lifecycle.recheck_at.startsWith('0001-') ? '<p>下次小时复查：'+deps.esc(lifecycle.recheck_at)+'；恢复周期：'+Number(lifecycle.cycle || 1)+'</p>' : '')+
                '<p><b>当前结论：</b>'+deps.esc(lifecycle.conclusion || '仍在收集证据')+'</p>'+
                '<p>成功样本：'+Number(lifecycle.successes)+'；整站试探轮数：'+Number(lifecycle.probe_attempts)+'</p>' : '')+
                (lifecycle ? '<p>触发失败：'+Number(report.failures)+'；当前窗口失败 / 成功：'+Number(lifecycle.window_failures)+' / '+Number(lifecycle.window_successes)+
                    '；短窗口失败：'+Number(lifecycle.short_failures)+'；已知失败任务：'+Number(lifecycle.distinct_tasks)+'</p>' : '')+
                '<p><strong>'+deps.esc(report.summary)+'</strong></p>'+
                '<p>'+deps.esc(report.complete?'样本数量与触发计数一致，未截断':'证据不完整，请结合下方说明排查')+'</p>'+
                (report.hypotheses && report.hypotheses.length ? '<p><b>结构化证据假设（未验证）：</b>'+deps.esc(report.hypotheses.join('；'))+'</p>' : '')+
                (report.contradictions && report.contradictions.length ? '<p><b>反证：</b>'+deps.esc(report.contradictions.join('；'))+'</p>' : '')+
                (report.missing && report.missing.length ? '<p><b>仍需验证：</b>'+deps.esc(report.missing.join('；'))+'</p>' : '')+
                (report.groups.length ? '<p><b>优先核查（未验证）：</b>'+deps.esc(report.groups[0].hypothesis || '')+'</p><p><b>验证步骤：</b>'+deps.esc(report.groups[0].verify || '')+'</p>' : '')+
                '<div class="breaker-analysis-table"><table><thead><tr><th>阶段</th><th>错误类型</th><th>次数</th><th>采样占比</th><th>任务数</th></tr></thead><tbody>'+rows+'</tbody></table></div>'+
                (stageRows ? '<h3>全阶段验证进度</h3><div class="breaker-analysis-table"><table><thead><tr><th>阶段</th><th>检查进度</th><th>验证方式与覆盖范围</th><th>验证结论</th><th>轮次</th><th>探针 / 真实任务成功</th><th>说明</th></tr></thead><tbody>'+stageRows+'</tbody></table></div>' : '')+
                (lifecycle && lifecycle.checks_limited ? '<p>阶段清单不完整，不能认定整站恢复。</p>' : '')+
                (lifecycle && lifecycle.counters_reset_at && !lifecycle.counters_reset_at.startsWith('0001-') ? '<p>运行计数已于 '+deps.esc(lifecycle.counters_reset_at)+' 清零；本报告保留清零前的统计证据。</p>' : '')+
                (timelineRows ? '<h3>运行时间线：暂停、试探与恢复</h3><ol>'+timelineRows+'</ol>' : '')+
                '<details><summary>展开完整证据与各组验证步骤</summary><pre class="log-tail-text"></pre></details>';
            body.querySelector('pre').textContent = report.text;
        }
        async function refresh() {
          if (!current()) return;
          try {
            var response = await deps.apiFetch('/api/breaker/analysis?scope='+encodeURIComponent(site),{current:current});
            if (!current()) return;
            if (!response) throw new Error('读取失败，请检查登录状态');
            if (!response.ok) throw new Error((await response.text()).trim() || '读取熔断分析失败');
            var data = await response.json();
            if (!current()) return;
            status.textContent = data.retention;
            if (!data.reports.length) {status.textContent += ' 当前没有记录；旧版本产生的熔断无法补录实时现场。';select.disabled=true;return;}
            select.innerHTML = data.reports.map(function(r,i){return '<option value="'+i+'">'+deps.esc(r.at)+' · '+r.failures+' 次'+(r.lifecycle?' · '+deps.esc(r.lifecycle.state):'')+'</option>';}).join('');
            select.disabled = false;
            var selectedIndex = selected ? data.reports.findIndex(function(r){return r.id===selected.id;}) : 0;
            if (selectedIndex < 0) selectedIndex = 0;
            select.value = String(selectedIndex);
            select.onchange = function(){render(data.reports[Number(select.value)]);};
            render(data.reports[selectedIndex]);
          } catch(error) {if(current()){status.textContent=error.message;select.disabled=true;}}
          finally {if(current())refreshTimer=setTimeout(refresh,3000);}
        }
        await refresh();
    }

    // 暂停抓取：只给 site = 整站那一层（框架自带界面走的就是这一档 —— 站点控制卡上那个按钮）；
    // 再给 stage = 只停那一个阶段（留给业务自己接的界面/脚本）。暂停**必须点名**（一个误点不该停掉所有站）。
    async function pauseCrawling(site, stage) {
        var body = { scope: site };
        if (stage) body.stage = stage;
        var resp = await deps.apiPost('/api/breaker/pause', body);
        if (!resp || !resp.ok) return;
        var data = null;
        try { data = await resp.json(); } catch (e) { /* 拿不到 body 不影响暂停结果 */ }
        if (data && data.paused === false) deps.Toast.info(stage ? '该阶段（或整站）本来就在暂停态' : '该站点本来就在暂停态');
        else deps.Toast.ok(stage ? ('已暂停 ' + site + '/' + stage) : ('已暂停 ' + site));
        deps.fetchData();
    }

    /* ============ 取数 ============ */
    // API 与监控轮询由 requests.js 提供。


function reset(){breakerAnalysisSeq++;bellSeen=null;var bell=deps.document.getElementById('bell'),badge=deps.document.getElementById('bell-badge'),panel=deps.document.getElementById('bell-panel');if(bell){bell.setAttribute('aria-expanded','false');bell.classList.remove('pulse');}if(badge){badge.textContent='0';badge.hidden=true;badge.className='bell-badge';}if(panel){panel.innerHTML='';panel.hidden=true;}}

function unmount(){breakerAnalysisSeq++;closeBell();}



return Object.defineProperties({}, {
"renderBell":{enumerable:true,get:function(){return renderBell;},set:function(value){renderBell=value;}},
"scopeKey":{enumerable:true,get:function(){return scopeKey;},set:function(value){scopeKey=value;}},
"pulseBell":{enumerable:true,get:function(){return pulseBell;},set:function(value){pulseBell=value;}},
"bellRow":{enumerable:true,get:function(){return bellRow;},set:function(value){bellRow=value;}},
"toggleBell":{enumerable:true,get:function(){return toggleBell;},set:function(value){toggleBell=value;}},
"closeBell":{enumerable:true,get:function(){return closeBell;},set:function(value){closeBell=value;}},
"resumeCrawling":{enumerable:true,get:function(){return resumeCrawling;},set:function(value){resumeCrawling=value;}},
"forceResumeCrawling":{enumerable:true,get:function(){return forceResumeCrawling;},set:function(value){forceResumeCrawling=value;}},
"renderSiteControl":{enumerable:true,get:function(){return renderSiteControl;},set:function(value){renderSiteControl=value;}},
"openBreakerAnalysis":{enumerable:true,get:function(){return openBreakerAnalysis;},set:function(value){openBreakerAnalysis=value;}},
"pauseCrawling":{enumerable:true,get:function(){return pauseCrawling;},set:function(value){pauseCrawling=value;}},
"reset":{enumerable:true,get:function(){return reset;},set:function(value){reset=value;}},
"unmount":{enumerable:true,get:function(){return unmount;},set:function(value){unmount=value;}}
});
});
MonitorShell.install("breakers/controller");
