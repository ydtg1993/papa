package server

import (
	"io/fs"
	"strings"
	"testing"
)

func TestQueuePanelHasGlobalQueues(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	js := string(raw)
	for _, want := range []string{
		"error_queue: { label: '错误队列'",  // 错误队列进种类表
		"repeat_queue: { label: '轮询队列'", // 轮询队列同样
	} {
		if !strings.Contains(js, want) {
			t.Errorf("mo.js 里缺少 %q —— 两条治理队列都要在面板种类表里", want)
		}
	}
	// 只钉**旧按站点拆队列**那两个特征串。这里曾经还有一条 `.site === currentSite` ——
	// 它不精确：按站点过滤是**正当**用法（阶段面板、阶段入口卡片都按它分），
	// 当初那条能过只是因为阶段过滤写的是 `(stages[name].site || '') === currentSite`，
	// 中间夹了个 `|| ''` 才没命中。守卫挡不住回归却又会误伤新代码，就不留它。
	for _, bad := range []string{"?site=' + encodeURIComponent", "parseQueue(name)"} {
		if strings.Contains(js, bad) {
			t.Errorf("mo.js 里不该再有按站点拆队列的残留 %q", bad)
		}
	}
}

func TestSiteControlPanelWired(t *testing.T) {
	tpl, err := fs.ReadFile(templateFS, "template.html")
	if err != nil {
		t.Fatalf("read template.html: %v", err)
	}
	if !strings.Contains(string(tpl), `id="dash-site-ctl"`) {
		t.Error(`template.html 里没有 id="dash-site-ctl" —— renderSiteControl 会静默什么都不画`)
	}
	js, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	for _, want := range []string{
		"renderSiteControl(",
		"pauseCrawling(",
		"resumeCrawling(",
		"/api/breaker/pause",
		"/api/breaker/resume",
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("mo.js 里缺少 %q", want)
		}
	}
	// 那一档是**整站**的暂停：站点控制卡上不该再出现点火那个动作（引擎侧已经没有它了）
	if strings.Contains(string(js), "triggerEntry(") || strings.Contains(string(js), "/api/entries/trigger") {
		t.Error("mo.js 里还留着点火那套东西（入口不再有手动模式）")
	}
}

func TestBellPanelWired(t *testing.T) {
	tpl, err := fs.ReadFile(templateFS, "template.html")
	if err != nil {
		t.Fatalf("read template.html: %v", err)
	}
	for _, want := range []string{`id="bell"`, `id="bell-badge"`, `id="bell-panel"`, `id="bell-wrap"`} {
		if !strings.Contains(string(tpl), want) {
			t.Errorf("template.html 里缺少 %s —— 铃铛/徽标/面板就接不上", want)
		}
	}
	if strings.Contains(string(tpl), "breaker-banner") {
		t.Error("template.html 里还留着 breaker-banner（整页红条已被铃铛取代）")
	}

	js, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	for _, want := range []string{
		"renderBell(",
		"toggleBell(",
		"closeBell()",
		"bell-badge",          // 徽标的数字与颜色都写在这个 class 上
		"b.source",            // 徽标颜色（熔断=红 / 手动=琥珀）与面板文案都靠它区分
		"/api/breaker/resume", // 面板里那两个「恢复」
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("mo.js 里缺少 %q", want)
		}
	}
	for _, bad := range []string{"renderBreaker(", "breaker-banner"} {
		if strings.Contains(string(js), bad) {
			t.Errorf("mo.js 里还留着 %q（整页红条已被铃铛取代）", bad)
		}
	}

	// 样式跟着钉一下：面板的定位/层叠写在 mo.css 里，删了它面板会以内联方式铺在页面上
	css, err := fs.ReadFile(staticFS, "static/mo.css")
	if err != nil {
		t.Fatalf("read mo.css: %v", err)
	}
	for _, want := range []string{".bell-panel", ".bell-badge", ".bell "} {
		if !strings.Contains(string(css), want) {
			t.Errorf("mo.css 里缺少 %q", want)
		}
	}
	if strings.Contains(string(css), ".breaker-banner") {
		t.Error("mo.css 里还留着 .breaker-banner")
	}
}

func TestMonitorResponseKeysMatchFrontend(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	js := string(raw)
	for _, want := range []string{"data.stages", "data.queues", "data.breakers", "data.sites", "data.system"} {
		if !strings.Contains(js, want) {
			t.Errorf("mo.js 里没读 %s —— 服务端写了这个键，前端却读别的名字就会静默失效", want)
		}
	}
	// 「自定义数据」模块已删除：服务端不再发 custom，前端也不许再读（读了会静默拿不到东西）。
	if strings.Contains(js, "data.custom") {
		t.Error("mo.js 还在读 data.custom —— 自定义数据模块已整块删除，服务端不再发这个键")
	}
	// 阶段面板的标题要读条目里的裸阶段名：快照的键是 `site:stage`（阶段名只在本站内唯一），
	// 拿键当标题在多站下会显示成 "shop:catalog"。同 `data.breakers`/`data.breaker` 那条，
	// 前端没有测试框架，只能这样钉住。
	if !strings.Contains(js, "st.stage") {
		t.Error("mo.js 的 stageLabel 没读 st.stage —— 标题会退回 `site:stage` 那个复合键")
	}
	// 横幅判"这次是熔断还是手动"必须读条目里的 `source`。曾经用 `enabled && stage` 猜 ——
	// 阶段级的手动暂停也带 stage（那是它的 scope），在熔断启用着的站上会被说成"熔断"。
	if !strings.Contains(js, "b.source") {
		t.Error("mo.js 的横幅没读 b.source —— 手动的阶段级暂停会被显示成「熔断」")
	}
	// 单数那个是错的（服务端从来不发这个键）
	for _, line := range strings.Split(js, "\n") {
		if strings.Contains(line, "data.breaker ") || strings.Contains(line, "data.breaker)") || strings.Contains(line, "data.breaker;") {
			t.Errorf("mo.js 读了不存在的 data.breaker（服务端发的是 data.breakers）：%s", strings.TrimSpace(line))
		}
	}
}

func TestDashboardQueueOverviewGroupsBySite(t *testing.T) {
	tpl, err := fs.ReadFile(templateFS, "template.html")
	if err != nil {
		t.Fatalf("read template.html: %v", err)
	}
	if !strings.Contains(string(tpl), `id="dash-queue"`) {
		t.Error("template.html 里缺少 id=\"dash-queue\" —— 概览卡片画不进去")
	}
	if !strings.Contains(string(tpl), "按站点分卡片") {
		t.Error("template.html 的「任务队列概览」注释没写清两种形态（概览=站点卡片 / 站点 Tab=多阶段详情）")
	}

	js, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	raw := string(js)
	for _, want := range []string{
		"function stageLine(",
		"function renderQueueSummary(stages, sites)",
		"queue-site-grid",
		"site-card-head",
		"stage-line",
		"renderQueueSummary(view.stages, view.sites)",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("mo.js 里缺少 %q —— 概览按站点分组会静默失效", want)
		}
	}
	// 只有「概览」才分组：选中站点 Tab 时必须回到完整的多阶段详情。
	if !strings.Contains(raw, "if (currentSite === null && sites && sites.length > 0)") {
		t.Error("mo.js 的 renderQueueSummary 没有按 currentSite 区分「概览=站点卡片 / 站点 Tab=完整详情」")
	}
	if !strings.Contains(raw, "五格大数字") {
		t.Error("mo.js 缺少「选中站点展示完整阶段详情」的注释说明 —— 后来者会误删这条分支")
	}

	css, err := fs.ReadFile(staticFS, "static/mo.css")
	if err != nil {
		t.Fatalf("read mo.css: %v", err)
	}
	for _, want := range []string{
		".queue-site-grid", ".site-card", ".site-card-head", ".stage-line", ".stage-line .mini",
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("mo.css 里缺少 %q —— 站点卡片会掉成无样式堆叠", want)
		}
	}
}

func TestQueueGovernanceKeepsDashboardAndDedicatedDetails(t *testing.T) {
	tpl, err := fs.ReadFile(templateFS, "template.html")
	if err != nil {
		t.Fatalf("read template.html: %v", err)
	}
	page := string(tpl)
	for _, gone := range []string{`data-module="queue"`, `id="mod-queue"`} {
		if strings.Contains(page, gone) {
			t.Errorf("template.html 里还有 %q —— 「任务队列」模块已并入 Dashboard，菜单/模块壳不该回来", gone)
		}
	}
	for _, want := range []string{`id="mod-queues"`, `id="queue-stages"`, `/static/queue-governance.js`, `/static/queue-governance.css`} {
		if !strings.Contains(page, want) {
			t.Errorf("队列治理详情接线缺失：%s", want)
		}
	}

	js, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	raw := string(js)
	for _, gone := range []string{"function renderQueue(", "renderQueue(view.stages)", "queue-stages", "queue: '任务队列'"} {
		if strings.Contains(raw, gone) {
			t.Errorf("mo.js 里还有 %q —— 独立「任务队列」模块的接线会重复画一份", gone)
		}
	}
	// 原模块的内容必须完整地搬过来：阶段 tiles 的累计行 + Worker 明细表。
	for _, want := range []string{
		"function stageDetail(",
		"function stageGlobal(",
		"function workerTable(",
		"累计任务 ",
		"Worker 明细",
		"暂无 Worker 明细",
		"Worker</th><th>任务</th><th>失败</th>",
		"stageDetail(name, stages[name])",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("mo.js 里缺少 %q —— 原「任务队列」模块的 Worker 明细会丢", want)
		}
	}

	css, err := fs.ReadFile(staticFS, "static/mo.css")
	if err != nil {
		t.Fatalf("read mo.css: %v", err)
	}
	if !strings.Contains(string(css), ".stage-workers") {
		t.Error("mo.css 里缺少 .stage-workers —— Worker 明细表会贴到累计行上")
	}
}

func TestCustomDataModuleRemoved(t *testing.T) {
	tpl, err := fs.ReadFile(templateFS, "template.html")
	if err != nil {
		t.Fatalf("read template.html: %v", err)
	}
	page := string(tpl)
	for _, gone := range []string{`data-module="custom"`, `id="mod-custom"`, `id="custom"`, "自定义数据"} {
		if strings.Contains(page, gone) {
			t.Errorf("template.html 里还有 %q —— 「自定义数据」模块已删除", gone)
		}
	}

	js, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	raw := string(js)
	for _, gone := range []string{"function renderCustom(", "data.custom", "自定义数据", "custom: '"} {
		if strings.Contains(raw, gone) {
			t.Errorf("mo.js 里还有 %q —— 服务端不再发 custom，前端留着只会静默失效", gone)
		}
	}
}
