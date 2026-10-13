package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
)

// entryFetcher 一个实现了 SubmitEntries 的假 fetcher：记下每次点火、可以按需阻塞（测串行化）。
//
// 不复用 recordingFetcher：给那个加上 SubmitEntries 会让**所有**用它的阶段都变成"有入口"，
// 把别的用例的语义一起改了。
type entryFetcher struct {
	stage string
	block chan struct{} // 非 nil 时 SubmitEntries 会一直等它（测并发点火）

	mu      sync.Mutex
	calls   int
	sites   []string
	inBlock bool // 已经进了回调、正卡在 block 上 —— 用例靠它确定"确实有人在跑了"
}

func (f *entryFetcher) GetStage() string { return f.stage }

func (f *entryFetcher) FetchHandler(context.Context, *Task, *Engine) error { return nil }

func (f *entryFetcher) SubmitEntries(_ *Engine, site core.Site) {
	if f.block != nil {
		f.mu.Lock()
		f.inBlock = true // 先亮灯再阻塞：用例据此确定"第一次已经在跑了"，否则会跟它抢锁
		f.mu.Unlock()
		<-f.block
	}
	f.mu.Lock()
	f.calls++
	f.sites = append(f.sites, site.Key)
	f.mu.Unlock()
}

// waitingInBlock 第一次点火是否已经卡在 block 上。
func (f *entryFetcher) waitingInBlock() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inBlock
}

func (f *entryFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// entryEngine 一台配好入口三态的实验引擎：auto / manual / off 各一个阶段（都实现了 SubmitEntries）。
func entryEngine(t *testing.T) (*Engine, *entryFetcher, *entryFetcher, *entryFetcher) {
	t.Helper()
	e := newTestEngine(t, newFakeTaskDB())
	auto, manual, off := &entryFetcher{stage: "auto"}, &entryFetcher{stage: "manual"}, &entryFetcher{stage: "off"}
	cfg := StageConfig{Site: "a", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}
	cfgAuto, cfgManual := cfg, cfg
	cfgAuto.Entry, cfgManual.Entry = EntryAuto, EntryManual
	e.AddStage("auto", cfgAuto, auto, func(eng *Engine) { auto.SubmitEntries(eng, core.Site{Key: "a"}) })
	e.AddStage("manual", cfgManual, manual, func(eng *Engine) { manual.SubmitEntries(eng, core.Site{Key: "a"}) })
	e.AddStage("off", cfg, off, func(eng *Engine) { off.SubmitEntries(eng, core.Site{Key: "a"}) })
	return e, auto, manual, off
}

// 三种模式在引擎侧的落地：auto 启动就投；manual 一次都不自动投；off 的回调在声明层就被丢掉了
// （这里模拟那个状态 —— 传 nil），所以它连"有入口"都不算。
func TestStageEntryModesAtStartup(t *testing.T) {
	e, auto, manual, _ := entryEngine(t)
	// off 在声明层就没了回调（internal/app 那边置 nil），引擎侧看到的就是"没入口"
	e.AddStage("off", StageConfig{Site: "a", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}, &entryFetcher{stage: "off"}, nil)

	e.ApplyRegisterStage()

	if auto.callCount() != 1 {
		t.Fatalf("auto 应当在启动时投一次，实得 %d", auto.callCount())
	}
	if manual.callCount() != 0 {
		t.Fatalf("manual 启动时一次都不该投（等后台点火），实得 %d", manual.callCount())
	}
	if got := e.StageEntries(); len(got) != 2 {
		t.Fatalf("应当只列出 auto 与 manual 两个阶段（off 的回调是 nil），实得 %+v", got)
	}
}

// 点火：manual 阶段点得动；auto / 没入口 / 未注册各有各的哨兵错误，文案要说清下一步怎么做。
func TestSubmitStageEntries(t *testing.T) {
	e, auto, manual, _ := entryEngine(t)
	e.AddStage("bare", StageConfig{Site: "a", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}, &recordingFetcher{stage: "bare"}, nil)
	e.ApplyRegisterStage()
	auto.calls, manual.calls = 0, 0 // 只关心点火这一下（启动那次不算）

	if err := e.SubmitStageEntries("a", "manual"); err != nil {
		t.Fatalf("manual 阶段应当点得动：%v", err)
	}
	if manual.callCount() != 1 {
		t.Fatalf("点火应当调一次 SubmitEntries，实得 %d", manual.callCount())
	}

	cases := []struct {
		name  string
		site  string
		stage string
		want  error
	}{
		{"没注册的阶段", "a", "ghost", ErrStageNotRegistered},
		{"注册了但没入口", "a", "bare", ErrStageNoEntry},
		{"auto 阶段不给点火", "a", "auto", ErrEntryAutoStage},
		{"站点对不上", "b", "manual", ErrStageNotRegistered},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := e.SubmitStageEntries(c.site, c.stage)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v，want %v", err, c.want)
			}
		})
	}
	// auto 的点火被拒了，就不该真的投出去
	if auto.callCount() != 0 {
		t.Fatalf("被拒的点火不该调到回调，实得 %d 次", auto.callCount())
	}

	// 点过之后 StageEntries 要带上时刻（前端"上次点火"那一列靠它）
	for _, en := range e.StageEntries() {
		if en.Site == "a" && en.Stage == "manual" {
			if en.LastTriggeredAt.IsZero() {
				t.Fatal("点过之后应当记下点火时刻")
			}
			if en.Mode != string(EntryManual) {
				t.Fatalf("mode = %q，want manual", en.Mode)
			}
		}
		if en.Stage == "auto" && !en.LastTriggeredAt.IsZero() {
			t.Fatal("启动时自动投的那一次不算手动点火，时刻该是零值")
		}
	}
}

// 引擎没走完 ApplyRegisterStage、或已经在停机：拒绝点火，而且**不能建池** ——
// 停机后建出来的池 stopCh 永远不会关，worker 就一直空转等闸门（谁也停不掉它）。
func TestSubmitStageEntriesRejectsWhenNotReady(t *testing.T) {
	e, _, manual, _ := entryEngine(t) // 故意不调 ApplyRegisterStage
	if err := e.SubmitStageEntries("a", "manual"); !errors.Is(err, ErrEntryNotReady) {
		t.Fatalf("没就绪时应当拒绝：%v", err)
	}
	if manual.callCount() != 0 {
		t.Fatal("被拒的点火不该调到回调")
	}

	e.ApplyRegisterStage()
	if err := e.SubmitStageEntries("a", "manual"); err != nil {
		t.Fatalf("就绪后应当点得动：%v", err)
	}

	e.cancel() // 进入停机
	if err := e.SubmitStageEntries("a", "manual"); !errors.Is(err, ErrEntryNotReady) {
		t.Fatalf("停机后应当拒绝：%v", err)
	}
	if pool := e.pools.get(core.StageKey("a", "manual")); pool != nil {
		t.Fatal("停机后的点火不该建出池子（那个池谁也停不掉）")
	}
}

// 点火串行化：一次还没跑完，第二次拿到 ErrEntryBusy（而不是排队等，把后台请求挂住）。
func TestSubmitStageEntriesSerializes(t *testing.T) {
	e, _, manual, _ := entryEngine(t)
	manual.block = make(chan struct{})
	e.ApplyRegisterStage()

	done := make(chan error, 1)
	go func() { done <- e.SubmitStageEntries("a", "manual") }()

	// 等第一次**真的进了回调**（它此时抱着锁卡在 block 上），再点第二次 ——
	// 不能等 callCount：那个数在阻塞之后才加，永远等不出来（早先就是这么写挂过一次）。
	deadline := time.Now().Add(2 * time.Second)
	for !manual.waitingInBlock() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !manual.waitingInBlock() {
		close(manual.block) // 让先头那次收尾，免得把整包测试挂住
		<-done
		t.Fatal("第一次点火没进回调 —— 点火没有被真的执行")
	}

	if err := e.SubmitStageEntries("a", "manual"); !errors.Is(err, ErrEntryBusy) {
		t.Fatalf("第二次点火应当拿到 ErrEntryBusy，实得 %v", err)
	}

	close(manual.block)
	if err := <-done; err != nil {
		t.Fatalf("第一次点火应当成功：%v", err)
	}
	if manual.callCount() != 1 {
		t.Fatalf("总共只该跑一次，实得 %d", manual.callCount())
	}
}

// StageEntries 的排序与字段：按站点 + 阶段名排，站点/阶段都是裸名（前端自己分组）。
func TestStageEntriesSorted(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	f := &entryFetcher{}
	for _, site := range []string{"b", "a"} {
		for _, stage := range []string{"detail", "catalog"} {
			cfg := StageConfig{Site: site, Entry: EntryManual, MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}
			e.AddStage(stage, cfg, f, func(*Engine) {})
		}
	}
	got := e.StageEntries()
	if len(got) != 4 {
		t.Fatalf("四个阶段都该列出来：%+v", got)
	}
	want := []string{"a:catalog", "a:detail", "b:catalog", "b:detail"}
	for i, en := range got {
		if key := core.StageKey(en.Site, en.Stage); key != want[i] {
			t.Fatalf("第 %d 条 = %s，want %s（顺序应当是站点 → 阶段）", i, key, want[i])
		}
	}
}
