package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/breaker"
)

// 后台 payload 的**字段集合**要钉住：这一版把"暂停状态"从熔断器搬进了暂停信号，
// `BreakerStatuses()` 变成了合成视图 —— 字段形状必须一个字都不少（只多一个 source），
// 否则横幅、站点卡片、外部脚本会在升级后静默少读一列。
//
// 两头一起钉：Go 这边 marshal 出来的 key 集合 == `docs/MONITOR.md` 里那份示例的 key 集合。
// 改字段就两边一起改，测试会拦住只改一边。
func TestBreakerStatusPayloadKeys(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: 5 * time.Minute, Threshold: 1})
	e.SetSiteBreaker("b", breaker.Config{Enabled: true, Window: 5 * time.Minute, Threshold: 50})
	e.ApplyRegisterStage()

	never := marshalKeys(t, e.BreakerStatuses()["b"]) // 从未暂停过

	// 手动暂停（来源 manual）
	if !e.PauseSite("b", "后台手动暂停") {
		t.Fatal("暂停应当成功")
	}
	manual := marshalKeys(t, e.BreakerStatuses()["b"])

	// 熔断触发（来源 breaker）：站点 a 的阈值是 1，一次终态失败就触发
	e.notifyFailure(t.Context(), &Task{Stage: "catalog", Site: "a", URL: "u"}, errors.New("boom"))
	if st := e.BreakerStatuses()["a"]; !st.Paused || st.Source != PauseSourceBreaker {
		t.Fatalf("熔断应当写进暂停信号并带上来源：%+v", st)
	}
	tripped := marshalKeys(t, e.BreakerStatuses()["a"])

	// 三种情形的 key 集合必须一致（"有时有、有时没有"会让前端静默读不到）
	want := strings.Join(never, ",")
	if strings.Join(manual, ",") != want || strings.Join(tripped, ",") != want {
		t.Fatalf("三种情形的 key 集合不一致：\n从未 %s\n手动 %s\n熔断 %s",
			want, strings.Join(manual, ","), strings.Join(tripped, ","))
	}

	// 与文档对账：取 MONITOR.md 里 `"breakers": [` 那段示例的 key
	doc := docKeys(t, "breakers")
	if strings.Join(manual, ",") != strings.Join(doc, ",") {
		t.Fatalf("payload 与 docs/MONITOR.md 的示例对不上：\n代码 %v\n文档 %v", manual, doc)
	}
}

// entries 的字段集合同样两头钉：`core.StageEntry` marshal 出来 == docs/MONITOR.md 的示例。
// 加这个字段时正是它拦住了"代码改了、文档没跟上"。
func TestStageEntryPayloadKeys(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})
	// 入口回调必须给：`StageEntries()` 正是靠"有没有回调"判这个阶段有没有入口
	//（生产里由声明层的 entryFunc 填，测试这里给个空实现）
	e.AddStage("catalog", StageConfig{Site: "a", Entry: EntryManual, MaxAttempts: 1, WorkerCount: 1, QueueSize: 8},
		&recordingFetcher{stage: "catalog"}, func(*Engine) {})
	e.ApplyRegisterStage()

	rows := e.StageEntries()
	if len(rows) != 1 {
		t.Fatalf("应当列出那个 manual 阶段：%+v", rows)
	}
	keys := marshalEntryKeys(t, rows[0])
	if got, want := strings.Join(keys, ","), strings.Join(docKeys(t, "entries"), ","); got != want {
		t.Fatalf("entries 的字段与 docs/MONITOR.md 的示例对不上：\n代码 %s\n文档 %s", got, want)
	}

	// 暂停之后字段集合不能变（"有时有、有时没有"会让前端静默读不到），且状态要跟着走
	if !e.PauseStage("a", "catalog", "只停这个阶段") {
		t.Fatal("阶段级暂停应当成功")
	}
	rows = e.StageEntries()
	if !rows[0].Paused || rows[0].PauseSource != PauseSourceManual {
		t.Fatalf("阶段级暂停应当反映在这一行上：%+v", rows[0])
	}
	if got := strings.Join(marshalEntryKeys(t, rows[0]), ","); got != strings.Join(keys, ",") {
		t.Fatalf("暂停前后字段集合不一致：\n前 %s\n后 %s", strings.Join(keys, ","), got)
	}
	// 整站暂停要**继承**到阶段行（闸门看到的就是它，两处必须一致）
	e.ResumeStage("a", "catalog")
	if !e.PauseSite("a", "整站") {
		t.Fatal("整站暂停应当成功")
	}
	if rows = e.StageEntries(); !rows[0].Paused || rows[0].PauseSource != PauseSourceManual {
		t.Fatalf("整站暂停应当继承到阶段行：%+v", rows[0])
	}
}

func marshalEntryKeys(t *testing.T, en core.StageEntry) []string {
	t.Helper()
	raw, err := json.Marshal(en)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func marshalKeys(t *testing.T, st BreakerStatus) []string {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// docKeys 从 docs/MONITOR.md 里某段 jsonc 示例抠出字段名（`"<marker>": [` 之后到那节的 ``` 为止）。
// 外层的键（如 "breakers"、"entries"）不算 —— 它们是容器名，不是这一项的字段。
func docKeys(t *testing.T, marker string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "docs", "MONITOR.md"))
	if err != nil {
		t.Fatalf("读 docs/MONITOR.md：%v", err)
	}
	body := string(raw)
	head := `"` + marker + `": [`
	start := strings.Index(body, head)
	if start < 0 {
		t.Fatalf("docs/MONITOR.md 里找不到 %s 的示例段", marker)
	}
	start += len(head) // 从 `[` 之后开始
	end := strings.Index(body[start:], "\n```")
	if end < 0 {
		t.Fatalf("%s 的示例段没有收尾", marker)
	}
	seen := map[string]bool{}
	for _, m := range docFieldRe.FindAllStringSubmatch(body[start:start+end], -1) {
		seen[m[1]] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var docFieldRe = regexp.MustCompile(`"([a-z_]+)":`)
