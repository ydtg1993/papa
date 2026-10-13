package app

import (
	"fmt"
	"strings"
	"sync"
)

// 本文件是**站点注册表**：项目里每个站点文件在自己的 `init()` 里调一次 `papa.RegisterSite(...)`，
// 框架自己收集，项目**不用手写汇总清单**（加一个站 = 加一个文件）。
//
// 为什么必须"文件自己登记"：Go 没有"枚举一个包里有哪些函数/类型"的能力（反射只能从值倒推名字），
// 所以"框架自动发现"只有两条路 —— 代码生成，或由代码自己登记。这里选后者：零构建步骤、
// 一眼可 grep（每个站点文件里都写着那一行），也是 `database/sql` 驱动、`image` 格式那套标准做法。
//
// 顺序：登记的先后 = **文件名字典序**（Go 规范：同一个包里 init 按文件名排序执行），稳定可复现。

var (
	registeredMu sync.Mutex
	registered   []SiteSpec
)

// RegisterSite 登记一个站点声明。**只在包的 init() 里调**（或包级变量初始化里）：
//
//	func init() { papa.RegisterSite(mysite()) }
//
// 同一个站点键登记两次会在注册时（App.RegisterSites）报错并点名 —— 那多半是两个文件写了同一个 Key。
func RegisterSite(site SiteSpec) {
	registeredMu.Lock()
	defer registeredMu.Unlock()
	registered = append(registered, site)
}

// Sites 返回**框架收集到的**全部站点声明（顺序 = 登记顺序，也就是文件名字典序）。
//
// 返回的是副本：调用方改它不会影响登记表。
func Sites() []SiteSpec {
	registeredMu.Lock()
	defer registeredMu.Unlock()
	return append([]SiteSpec(nil), registered...)
}

// resetRegisteredSites 清空登记表（只给测试用：全局登记表是进程级的，用例之间要互不影响）。
func resetRegisteredSites() {
	registeredMu.Lock()
	defer registeredMu.Unlock()
	registered = nil
}

// validateSites 校验一批站点声明（纯函数，便于单测）：站点键**必填**、不能重复、也不能带 `:`。
func validateSites(sites []SiteSpec) error {
	seen := make(map[string]bool, len(sites))
	for i, site := range sites {
		// Key 必填：阶段键是 `site:stage`，熔断 / 统计 / 任务表也都按站点分 —— 没有 Key 就分不开。
		// 没有「未归属（默认 scope）」那一档：任务的 site 同样是必填的（SubmitTask 拒空串），
		// 所以未命名的声明只会是「注册上了、却永远收不到新任务」的陷阱，在声明期就拒掉。
		if site.Key == "" {
			return fmt.Errorf("第 %d 个站点声明缺 Key —— 阶段名按站点作用域（键是 `site:stage`），"+
				"熔断、统计、任务表也都按站点分，没有 Key 就分不开。站点是必填维度："+
				"没有「未归属」那一档（声明要有 Key，任务要有 site）", i+1)
		}
		// 阶段键是 `site:stage` 拼出来的，Key 里再有 `:` 就会串味：Key `shop:eu` + 阶段
		// `catalog` 与 Key `shop` + 阶段 `eu:catalog` 会拼成同一个键 —— 两对不同的东西共用一个
		// 池子、一把闸门、一行统计。这是静默串站，不如声明时就拒绝。
		if strings.Contains(site.Key, ":") {
			return fmt.Errorf("站点键 %q 不能包含 `:`（第 %d 个）—— 阶段键按 `site:stage` 拼，"+
				"键里带 `:` 会和别的站点/阶段拼出同一个键", site.Key, i+1)
		}
		if seen[site.Key] {
			return fmt.Errorf("站点键 %q 重复（第 %d 个）—— 同一个站只能声明一次，"+
				"多半是两个文件写了同一个 Key", site.Key, i+1)
		}
		seen[site.Key] = true
	}
	return nil
}
