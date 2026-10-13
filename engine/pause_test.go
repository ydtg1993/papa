package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/breaker"
)

// 暂停信号的三种查询语义（别混用）：精确 scope（闸门与提交点）· 该站任意一条（后台状态）· 全部（ResumeCrawling）。
func TestPauseScopeSemantics(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})
	e.SetSiteBreaker("b", breaker.Config{Enabled: false, Threshold: 1})

	// 整站暂停：该站**任何阶段**都停，别的站不受影响
	if !e.PauseSite("a", "整站") {
		t.Fatal("暂停整站应当成功")
	}
	for _, stage := range []string{"catalog", "detail", "episode"} {
		if !e.isPaused(pauseScope{Site: "a", Stage: stage}) {
			t.Fatalf("整站暂停应当涵盖阶段 %q", stage)
		}
	}
	if e.isPaused(pauseScope{Site: "b", Stage: "catalog"}) {
		t.Fatal("不该影响别的站")
	}

	// 阶段级暂停：只停那一个阶段，兄弟阶段照跑
	if !e.PauseStage("b", "catalog", "只停这个阶段") {
		t.Fatal("阶段级暂停应当成功")
	}
	if !e.isPaused(pauseScope{Site: "b", Stage: "catalog"}) {
		t.Fatal("那个阶段应当停")
	}
	if e.isPaused(pauseScope{Site: "b", Stage: "detail"}) {
		t.Fatal("兄弟阶段不该被带上")
	}
	if info := e.pauseInfoOf("b"); !info.Paused || info.Scope.Stage != "catalog" {
		t.Fatalf("阶段级条目也要能被「该站有没有暂停」查到、并说清是哪个阶段（后台那一行靠它）：%+v", info)
	}

	// 空站点**不是通配**：它是旧行的遗留，不代表"所有站"
	if e.isPaused(pauseScope{Site: "", Stage: "catalog"}) {
		t.Fatal("空站点不该被任何暂停命中（它不通配）")
	}
	if e.PauseSite("", "空键不是站点") || e.PauseStage("", "catalog", "x") {
		t.Fatal("空站点不是合法 scope")
	}

	// 放行：ResumeSite 清该站的**所有**条目（整站 + 各阶段），否则界面上会"卡住且没出路"
	if !e.ResumeSite("b") {
		t.Fatal("放行应当成功")
	}
	if e.isPaused(pauseScope{Site: "b", Stage: "catalog"}) {
		t.Fatal("阶段条目也应当被清掉")
	}
	if e.pauseInfoOf("b").Paused {
		t.Fatal("放行后该站不该还有条目")
	}
	if !e.isPaused(pauseScope{Site: "a", Stage: "catalog"}) {
		t.Fatal("放行 b 不该动 a")
	}
}

// 卡片上那个「暂停」是**整站那一层**：它管这一站的全部阶段 ——
// 包括没有入口行、后台列表里根本看不到的那些阶段（detail / episode 这种由上一级投出来的）。
// 这条把那个语义钉住：站点级暂停之后，另一个阶段的提交被寄放、它的池子也不建。
func TestSitePauseCoversEveryStageOfThatSite(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := newTestEngine(t, f)
	e.SetSiteBreaker("stub", breaker.Config{Enabled: false, Threshold: 1})
	// 两个阶段：catalog 是入口阶段（后台卡片上有它），detail 不是（卡片上根本没有这一行）
	e.AddStage("catalog", StageConfig{Site: "stub", Entry: EntryAuto, MaxAttempts: 1, WorkerCount: 1, QueueSize: 8},
		&recordingFetcher{stage: "catalog"}, func(*Engine) {})
	detail := &recordingFetcher{stage: "detail"}
	e.AddStage("detail", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}, detail, nil)
	e.ApplyRegisterStage()

	if !e.PauseSite("stub", "整站停一下") {
		t.Fatal("暂停整站应当成功")
	}

	// 闸门这一层：另一个阶段也停着
	if !e.isPaused(pauseScope{Site: "stub", Stage: "detail"}) {
		t.Fatal("整站暂停必须涵盖该站的每一个阶段")
	}
	// 入队点这一层：往那个阶段投任务会被寄放（不建池、不跑）
	if err := e.SubmitTask(&Task{Stage: "detail", Site: "stub", URL: "https://example.com/d"}); err != nil {
		t.Fatalf("暂停中提交不该报错（寄放）：%v", err)
	}
	if got := e.spillBacklog(); got != 1 {
		t.Fatalf("应当寄放 1 条，实得 %d", got)
	}
	if pool := e.pools.get(core.StageKey("stub", "detail")); pool != nil {
		t.Fatal("暂停期间不该为那个阶段建池")
	}
	if got := detail.calls(); len(got) != 0 {
		t.Fatalf("暂停中不该跑任务，实得 %v", got)
	}
}

// 人工暂停**不跨重启**：进程重启即恢复运行（起进程本身就是一次人工介入）。
//
// 两边都要成立：新引擎的内存里没有暂停（构造上就没有），而库里残留的 true 会被启动播种清掉
// （`seedSiteRows` 那半边在 breakerstate_test.go 里另有专测）。
func TestPauseDoesNotSurviveRestart(t *testing.T) {
	f := newFakeTaskDB()
	first := siteEngine(t, f)
	first.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})
	first.seedSiteRows()
	if !first.PauseSite("a", "人工暂停") {
		t.Fatal("暂停应当成功")
	}

	// "重启"：同一份库上起一台新引擎
	second := siteEngine(t, f)
	second.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})
	second.seedSiteRows()

	if second.isPaused(pauseScope{Site: "a", Stage: "catalog"}) {
		t.Fatal("重启后不该还停着（人工暂停不跨重启）")
	}
	if got := second.BreakerStatuses()["a"]; got.Paused {
		t.Fatalf("重启后快照也不该显示暂停：%+v", got)
	}
}

// 闸门：暂停时阻塞、**状态一变就被唤醒**（不靠定时器）、停机时返回 false。
//
// 还要钉住"被无关变更唤醒后重查并重新阻塞" —— 广播是全局的，醒来必须重新判定自己这一条；
// 缓存 channel 就会变成热自旋（那条路在 -race 下也看不出来，只能靠行为断言）。
func TestPauseGateWakesAndReparks(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})
	e.SetSiteBreaker("b", breaker.Config{Enabled: false, Threshold: 1})

	gate := e.pauseGateFor(pauseScope{Site: "a", Stage: "catalog"})
	stop := make(chan struct{})
	done := make(chan bool, 1)
	go func() { done <- gate.Wait(stop) }()

	// 没暂停：立刻放行
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("没暂停时 Wait 应当返回 true")
		}
	case <-time.After(time.Second):
		t.Fatal("没暂停时不该阻塞")
	}

	// 暂停后再问：阻塞
	e.PauseSite("a", "停了")
	done = make(chan bool, 1)
	go func() { done <- gate.Wait(stop) }()
	select {
	case <-done:
		t.Fatal("暂停中 Wait 不该返回")
	case <-time.After(50 * time.Millisecond):
	}

	// 别的站变了：唤醒 → 重查 → 自己还停着 → 重新阻塞（不是放行，也不是空转）
	if !e.PauseSite("b", "别的站停了") {
		t.Fatal("暂停 b 应当成功")
	}
	select {
	case ok := <-done:
		t.Fatalf("被无关变更唤醒后不该放行（Wait = %v）", ok)
	case <-time.After(50 * time.Millisecond):
	}

	// 放行 a：这次要真的返回 true
	e.ResumeSite("a")
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("放行后 Wait 应当返回 true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("放行没有唤醒 Wait")
	}

	// 停机：**暂停中**的 Wait 必须能退出去，否则关停被暂停卡死
	if !e.PauseSite("a", "再停一次") {
		t.Fatal("再停一次应当成功")
	}
	done = make(chan bool, 1)
	go func() { done <- gate.Wait(stop) }()
	time.Sleep(20 * time.Millisecond)
	close(stop)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("stop 关闭后应当返回 false（该停机了）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop 没有唤醒 Wait")
	}
}

// 池子**建在暂停之后**也要冻住：闸门是按 scope 现查的，不是建池那一刻的快照。
func TestPoolBuiltWhilePausedIsFrozen(t *testing.T) {
	f := newFakeTaskDB()
	e := newTestEngine(t, f)
	e.SetSiteBreaker("stub", breaker.Config{Enabled: false, Threshold: 1})
	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}, fetcher, nil)
	e.ApplyRegisterStage()

	if !e.PauseSite("stub", "先别跑") {
		t.Fatal("暂停应当成功")
	}
	// 暂停期间把池子建起来（真实路径是恢复后第一条任务懒建），直接投一条进队列
	pool := e.getOrCreatePool(stubKey)
	if pool == nil {
		t.Fatal("阶段应当已登记")
	}
	if err := pool.Submit(&Task{ID: 1, Stage: "stub", Site: "stub", URL: "https://example.com/x"}); err != nil {
		t.Fatalf("Submit = %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if got := fetcher.calls(); len(got) != 0 {
		t.Fatalf("暂停期间建的池子也必须冻住，实得 %v", got)
	}

	if !e.ResumeSite("stub") {
		t.Fatal("放行应当成功")
	}
	fetcher.waitCalls(t, 1)
}

// 暂停中提交：**在入队点收口** —— 不入队、寄放到溢出表、不建池；行已经落库且是 pending。
func TestSubmitWhilePausedParksInsteadOfEnqueueing(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 全新任务：走 INSERT
	e := newTestEngine(t, f)
	e.SetSiteBreaker("stub", breaker.Config{Enabled: false, Threshold: 1})
	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}, fetcher, nil)
	e.ApplyRegisterStage()

	if !e.PauseSite("stub", "暂停") {
		t.Fatal("暂停应当成功")
	}
	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/x"}); err != nil {
		t.Fatalf("暂停中提交不该报错（任务寄放，恢复后跑）：%v", err)
	}

	if got := e.spillBacklog(); got != 1 {
		t.Fatalf("应当寄放 1 条，实得 %d", got)
	}
	if pool := e.pools.get(stubKey); pool != nil {
		t.Fatal("暂停期间不该建池（收口在建池之前）")
	}
	if !strings.Contains(f.written(), "`status`") {
		t.Fatalf("任务的行要先落库（pending），否则恢复/重启都找不回来：\n%s", f.written())
	}
	if got := fetcher.calls(); len(got) != 0 {
		t.Fatalf("暂停中不该跑任务，实得 %v", got)
	}
}

// 暂停的桶在回灌时**整桶跳过**，而且**不查库** —— 这是本次改动的主要收益：
// 以前闸住的站，每 drain_interval（默认 2s）对每条溢出任务做一次 SELECT + 一次 UPDATE，
// 而入队又判队满 → 再溢出，永远推进不了。
func TestDrainSpilledSkipsPausedBucketWithoutQuerying(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := newTestEngine(t, f)
	e.SetSiteBreaker("stub", breaker.Config{Enabled: false, Threshold: 1})
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}, &recordingFetcher{stage: "stub"}, nil)
	e.ApplyRegisterStage()
	e.PauseSite("stub", "暂停")
	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/x"}); err != nil {
		t.Fatal(err)
	}

	before := f.readSQL()
	e.drainSpilled()
	if got := f.readSQL(); got != before {
		t.Fatalf("暂停中的桶不该被查库（空转就是这么来的）：\n%s", got)
	}
	if got := e.spillBacklog(); got != 1 {
		t.Fatalf("整桶应当放回溢出表，实得 %d", got)
	}
}

// 恢复要**立刻**灌回，不等下一个 drain tick（drain_interval 可以配到小时）。
func TestResumeWakesDrainImmediately(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := newTestEngine(t, f) // 夹具把 DrainInterval 设成 1 小时：等 tick 就是永远等不到
	e.SetSiteBreaker("stub", breaker.Config{Enabled: false, Threshold: 1})
	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{Site: "stub", WorkerCount: 1, QueueSize: 8}, fetcher, nil)
	e.ApplyRegisterStage()
	e.PauseSite("stub", "暂停")
	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/x"}); err != nil {
		t.Fatal(err)
	}

	f.noRows = false // 回灌要按 id 回查那一行（真库里本来就在）
	if !e.ResumeSite("stub") {
		t.Fatal("放行应当成功")
	}
	fetcher.waitCalls(t, 1)
}

// 手动暂停一条**已经熔断**的站：返回 false 且不覆盖触发现场 ——
// 后台"空点不记审计"那条规则、以及"熔断现场不该被一次点击改写成手动"都靠它。
func TestManualPauseDoesNotOverwriteTripSnapshot(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1})
	e.ApplyRegisterStage()

	// 触发一次熔断（阈值 1）
	e.notifyFailure(context.Background(),
		&Task{Stage: "catalog", Site: "a", URL: "u"}, errors.New("boom"))
	trip := e.BreakerStatuses()["a"]
	if !trip.Paused || trip.Source != PauseSourceBreaker || trip.Stage != "catalog" {
		t.Fatalf("熔断应当写进暂停信号：%+v", trip)
	}

	if e.PauseSite("a", "人工也来一下") {
		t.Fatal("已经停着时 PauseSite 应当返回 false")
	}
	after := e.BreakerStatuses()["a"]
	if after.Source != PauseSourceBreaker || after.Stage != "catalog" || after.Reason != trip.Reason ||
		!after.PausedAt.Equal(trip.PausedAt) {
		t.Fatalf("手动暂停不该覆盖熔断现场：before=%+v after=%+v", trip, after)
	}
}

// 熔断**关着**时手动暂停照样有效：关掉的是"自动计数 + 触发"，与"能不能手动停"是两件事
// （业务拿它当维护窗口用）。这条原来在 internal/breaker 里，现在归暂停信号管。
func TestPauseWorksWithBreakerDisabled(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})

	if !e.PauseSite("a", "维护窗口") {
		t.Fatal("熔断关着也该能手动暂停")
	}
	if !e.isPaused(pauseScope{Site: "a", Stage: "catalog"}) {
		t.Fatal("暂停应当生效")
	}
	st := e.BreakerStatuses()["a"]
	if st.Enabled || !st.Paused || st.Source != PauseSourceManual {
		t.Fatalf("状态应当是「熔断关着、但被手动停着」：%+v", st)
	}
	if !e.ResumeSite("a") || e.isPaused(pauseScope{Site: "a", Stage: "catalog"}) {
		t.Fatal("放行应当生效")
	}
}
