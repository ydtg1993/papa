package config

import (
	"strings"
	"testing"
)

// 「是不是想写 X」必须是**确定**的：原来遍历的是 map，而这里取的是"距离最小的那个" ——
// 两个候选距离相同时谁赢就随机（叶子 `d` 距离 `dsn` 与 `dir` 都是 2），
// 于是同一条命令每次跑出来的提示都不一样，测试也可能偶发红。
func TestSuggestKeysIsDeterministic(t *testing.T) {
	for _, bad := range []string{
		"crawler.d",                            // 叶子 d：dsn 与 dir 并列距离 2
		"crawler.targt",                        // 没有相近的键，应当稳定地给不出建议
		"crawler.stages[catalog].wroker_count", // 嵌套 map 里的拼写错误
	} {
		first := suggestKeys([]string{bad})
		for i := 0; i < 50; i++ {
			if got := suggestKeys([]string{bad}); got != first {
				t.Fatalf("%s 的建议不稳定：第 %d 次 %q，首次 %q", bad, i+1, got, first)
			}
		}
	}

	// 前置条件：`d` 那条确实能凑出并列候选 —— 不成立的话上面就没在测"并列"这件事
	if suggestKeys([]string{"crawler.d"}) == "" {
		t.Fatal("前置条件：crawler.d 应当能给出建议（dsn / dir 并列）")
	}
}

// 未知键**按字典序报出**：这条是钉契约，不是钉已复现的 bug ——
// 把排序去掉之后 50 次里没跑出过不一致（`md.Unused` 的顺序在实测中看起来是稳的，
// 但它来自走 yaml 解出来的 map，没有文档保证）。排序给的是"不管上游怎么排都稳"，
// 代价只有一次 sort；顺带让这条报错和 `TestSuggestKeysIsDeterministic` 一样可断言。
func TestUnknownKeyMessageOrderIsStable(t *testing.T) {
	body := baseConfig + "zzz: 1\ncrawler:\n  targt: x\n"
	first := panicOn(t, body)
	for i := 0; i < 50; i++ {
		if got := panicOn(t, body); got != first {
			t.Fatalf("未知键报出顺序不稳定：第 %d 次\n%s\n首次\n%s", i+1, got, first)
		}
	}
	// 排序后 crawler.targt 应当排在 zzz 之前
	if strings.Index(first, "crawler.targt") > strings.Index(first, `"zzz"`) {
		t.Fatalf("应当按字典序报出，实得：\n%s", first)
	}
}

// 多条同时越界时，报哪一条必须是确定的（原来用的是 map，遍历顺序随机）。
func TestValidateRuntimeReportsFirstDeclaredField(t *testing.T) {
	neg := -1
	rt := &RuntimeConfig{
		ErrorQueue:   RuntimeErrorQueueConfig{WorkerCount: &neg, MaxRetry: &neg, BatchSize: &neg},
		RecoverQueue: RuntimeRecoverQueueConfig{WorkerCount: &neg},
		RepeatQueue:  RuntimeRepeatQueueConfig{WorkerCount: &neg},
	}
	for i := 0; i < 50; i++ {
		err := ValidateRuntime(rt)
		if err == nil || !strings.Contains(err.Error(), "error_queue.worker_count") {
			t.Fatalf("应当稳定地先报声明顺序里的第一条，实得 %v", err)
		}
	}
}
