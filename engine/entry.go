package engine

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/core"
)

// 本文件是**阶段入口**的声明方式。
//
// 起因：阶段原来只能在 main.go 里一个个注册（fetcher 值 + 入口回调各占一段），
// 阶段一多 main.go 就变成一堆装配代码。
// 更好的形状是**站点与阶段声明各站一个文件**（脚手架生成的 `configs/sites/<站名>.go`），
// main.go 只剩一行：
//
//	app.RegisterSites(papa.Sites()...)
//
// 那样的话"这个阶段的入口任务是什么"就不该再由 main.go 传进来 —— 让 fetcher 自己实现这个接口：
// 它是这个阶段自己的事，和 `GetStage()` 一样属于阶段的自述。

// EntrySubmitter 由 fetcher **可选**实现：本阶段投一批入口任务（列表页入口、分类入口……）。
//
// 什么时候投由**声明**说了算（`StageSpec.Entry`，见 EntryMode）：`auto` 在 ApplyRegisterStage
// 那一趟启动就投；`manual` 一次都不自动投，等后台「阶段入口」卡片上点一下
// （`Engine.SubmitStageEntries`）。两种模式都要求这个接口被实现。
//
// 引擎在 ApplyRegisterStage 的第三趟调用它 —— 那时**所有**阶段的池子都已备好
// （池子是懒建的，投给谁就等于把谁建起来），所以入口任务可以投给任意阶段，不必只在"自己这个阶段"上打转。
//
// site 是本阶段的**站点声明快照**（`SiteSpec` 压成的 `core.Site`：BaseURL / Headers…）。
// 站点值声明在 `configs/sites/<站名>.go` 一处，框架把它交到这里 —— fetcher 里不必再存副本：
// 存了就会漂（地址改了，fetcher 那份不会跟着变）。
//
//	没实现这个接口、或实现返回后什么也不投：都是合法的，阶段照常跑（只是没有入口任务）。
type EntrySubmitter interface {
	SubmitEntries(engine *Engine, site core.Site)
}

// EntryMode 阶段入口的模式：启动时投不投、后台给不给"点火"。
//
// 零值 `EntryOff` 表示"本阶段没有入口任务这回事"。**它只在声明层有意义**：
// 声明成 off 时 `internal/app` 会把入口回调整块丢掉（与"fetcher 没实现 SubmitEntries"
// 落到同一个状态）—— 所以引擎侧见到的只有 auto / manual 两种，
// 后台也不会看到 off 的阶段（列出来了却点不动，比不列更糟）。
type EntryMode string

const (
	// EntryOff 零值：不声明就是它。启动不投、后台也不列。
	EntryOff EntryMode = ""
	// EntryAuto 启动即投（`ApplyRegisterStage` 那一趟）。
	EntryAuto EntryMode = "auto"
	// EntryManual 启动不投，等后台点火。
	EntryManual EntryMode = "manual"
)

// String 给日志与后台用：零值空串在消息里读不出来，显示成 "off"。
func (m EntryMode) String() string {
	if m == EntryOff {
		return "off"
	}
	return string(m)
}

// Valid 声明期校验。`EntryMode` 是字符串枚举，"Auto" / "manual " 这种手误**能过编译**，
// 只会在运行时被当成"不是 auto"而静默不投 —— 所以声明层必须显式查一遍。
func (m EntryMode) Valid() error {
	switch m {
	case EntryOff, EntryAuto, EntryManual:
		return nil
	}
	return fmt.Errorf("Entry 只能是 %q / %q / %q（或不写 = %q），实得 %q",
		EntryAuto, EntryManual, EntryOff, EntryOff, string(m))
}

// 入口相关的哨兵错误：`Errors.Is` 判定，后台据此翻状态码（与 ErrStageNotRegistered 同一套用法）。
var (
	// ErrStageNoEntry 这个阶段没有入口任务（fetcher 没实现 EntrySubmitter）。
	ErrStageNoEntry = errors.New("该阶段没有入口任务（它的 fetcher 没有实现 SubmitEntries）")
	// ErrEntryAutoStage 这个阶段的入口是自动模式：启动时已经投过一轮。
	// 手动点火只对 manual 开放 —— 想随时手动点火就把它声明成 EntryManual。
	ErrEntryAutoStage = errors.New("该阶段的入口是 auto（启动时已自动投过）；要手动点火就把声明改成 Entry: papa.EntryManual")
	// ErrEntryNotReady 引擎还没走完 ApplyRegisterStage，或已经在停机。
	// 这两种时候点火都会踩坑：前者拿不到已解析的下载器覆盖层，后者会**建出一个谁也停不掉的池**
	//（Stop 早把活池表快照走了），投出来的行卡在 pending、而恢复队列下一轮又不会捞它们。
	ErrEntryNotReady = errors.New("引擎还没就绪（或正在停机），现在不能投入口任务")
	// ErrEntryBusy 已经有一次点火在进行中。点火是串行的（同一时刻只允许一次），
	// 连点或脚本并发触发时后来者拿这个错误，而不是排队等 —— 免得把后台请求挂住。
	ErrEntryBusy = errors.New("已有一次入口点火在进行中，稍后再试")
)

// SubmitStageEntries 手动点火：把这个阶段的入口任务投一轮（后台「阶段入口」卡片的那个按钮）。
//
// 它做的事就是启动那一趟做的事 —— 调 fetcher 的 `SubmitEntries`。**幂等**：入口任务自带幂等键，
// 已经有终态行（success/failed）的入口会被 SubmitTask 静默跳过，所以这里**不回报投了几条**，
// 它保证的是"把还没有终态行的入口投一遍"。要有确切条数得让 EntrySubmitter 回报计数（改接口，另说）。
//
// 串行化用 TryLock 而不是 Lock：fetcher 的 SubmitEntries 里往别的阶段投任务是**被鼓励**的用法
// （见 docs/FETCHER_WRITING_GUIDE.md §7.1），万一它投的正是另一个入口阶段、又回头调到这里，
// 阻塞版会把自己锁死。拿不到锁就回 ErrEntryBusy，让调用方（或操作人）重来。
//
// 站点被暂停时点火是**故意允许**的：任务照常入库，但**寄放在溢出表**里（不接新活、也不占 worker），
// 点「恢复」时立刻灌回 —— 这正是"先把入口排好、再放开这一站"的用法。
func (e *Engine) SubmitStageEntries(site, stage string) error {
	key := core.StageKey(site, stage)
	if err := e.entryReadyErr(); err != nil {
		return err
	}
	info := e.stages[key]
	if info == nil {
		return fmt.Errorf("%w: %s", ErrStageNotRegistered, key)
	}
	if info.submitFunc == nil {
		return fmt.Errorf("%w: %s", ErrStageNoEntry, key)
	}
	if info.config.Entry != EntryManual {
		// auto 与 off 都拦在这里：off 的阶段在声明层就被丢掉了回调（走不到这），
		// 所以实际拦的是 auto —— 它的入口已经在启动时投过了。
		return fmt.Errorf("%w: %s（当前 Entry=%s）", ErrEntryAutoStage, key, info.config.Entry)
	}
	if !e.entryMu.TryLock() {
		return ErrEntryBusy
	}
	defer e.entryMu.Unlock()
	// 拿到锁之后再确认一次：等锁的这一小段时间里引擎可能已经进了停机流程。
	if err := e.entryReadyErr(); err != nil {
		return err
	}
	info.submitFunc(e)
	e.markEntryTriggered(key)
	return nil
}

// StageEntries 列出**有入口任务**的阶段（auto 与 manual 都列，off 的连回调都没了、天然不在），
// 按站点 + 阶段名排序，并带上**这个阶段现在的暂停状态**。后台的「阶段入口」卡片读它。
func (e *Engine) StageEntries() []core.StageEntry {
	e.mu.RLock()
	out := make([]core.StageEntry, 0, len(e.stages))
	for key, info := range e.stages {
		if info.submitFunc == nil || info.config.Entry == EntryOff {
			continue
		}
		pause, paused := e.pauseStateOfScope(pauseScope{Site: info.config.Site, Stage: info.stage})
		out = append(out, core.StageEntry{
			Site:            info.config.Site,
			Stage:           info.stage,
			Mode:            info.config.Entry.String(),
			LastTriggeredAt: e.entryTriggeredAt(key),
			Paused:          paused,
			PauseSource:     pause.Source,
		})
	}
	e.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Site != out[j].Site {
			return out[i].Site < out[j].Site
		}
		return out[i].Stage < out[j].Stage
	})
	return out
}

// entryReadyErr 点火的前置条件：走完 ApplyRegisterStage 且没在停机。
//
// 为什么这么严：这两件事都只在"引擎真的活着"时才对。
// 未就绪时下载器的站点/阶段覆盖层还没解析（`resolveStageDownloads` 在 ApplyRegisterStage 里跑），
// 阶段依赖也没校验过；停机后再建池，那个池的 stopCh 永远不会被关，worker 就一直空转等闸门。
func (e *Engine) entryReadyErr() error {
	if !e.entriesReady.Load() {
		return ErrEntryNotReady
	}
	if e.ctx != nil && e.ctx.Err() != nil {
		return ErrEntryNotReady
	}
	return nil
}

// markEntryTriggered / entryTriggeredAt 记/读"上次手动点火"的时刻。
//
// **另用一把锁**，不跟 entryMu 复用：点火可能跑很久（每个入口 URL 都要查库/入库），
// 而复用的话整张 /api/monitor（每 3 秒刷一次）会跟着一起等它。
func (e *Engine) markEntryTriggered(key string) {
	e.entryLastMu.Lock()
	defer e.entryLastMu.Unlock()
	if e.entryLast == nil {
		e.entryLast = make(map[string]time.Time)
	}
	e.entryLast[key] = time.Now()
}

func (e *Engine) entryTriggeredAt(key string) time.Time {
	e.entryLastMu.Lock()
	defer e.entryLastMu.Unlock()
	return e.entryLast[key]
}
