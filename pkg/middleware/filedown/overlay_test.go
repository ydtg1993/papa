package filedown

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// 叠加层：nil = 不动；写了就连零值也认（0 并发、false 这些都得能表达）。
func TestOverlayApply(t *testing.T) {
	base := DefaultConfig()
	if got := (Overlay{}).Apply(*base); !reflect.DeepEqual(got, *base) {
		t.Fatalf("空覆盖层不该改动任何字段\n got=%+v\nwant=%+v", got, *base)
	}

	dir, state := "out", "state"
	retries, interval, timeout, concurrent, batch, queue := 0, 7, 45, 0, 3, 8
	var chunk int64 = 1 << 20
	resume, keep := false, true
	o := Overlay{
		OutputDir: &dir, ResumeStateDir: &state, MaxRetries: &retries, RetryInterval: &interval,
		Timeout: &timeout, MaxConcurrent: &concurrent, ChunkSize: &chunk, EnableResume: &resume,
		SaveBatchSize: &batch, KeepSegmentsAfterMerge: &keep, QueueSize: &queue,
	}
	got := o.Apply(*base)
	if got.OutputDir != dir || got.ResumeStateDir != state || got.MaxRetries != 0 ||
		got.RetryInterval != 7 || got.Timeout != 45*time.Second || got.MaxConcurrent != 0 ||
		got.ChunkSize != 1<<20 || got.EnableResume != false || got.SaveBatchSize != 3 ||
		got.KeepSegmentsAfterMerge != true || got.QueueSize != 8 {
		t.Fatalf("覆盖结果不对：%+v", got)
	}
	if base.OutputDir == dir {
		t.Fatal("Apply 不该改动传入的 Config")
	}
}

func TestOverlayThenStageWins(t *testing.T) {
	siteDir, stageDir := "site", "stage"
	concurrent := 5
	merged := Overlay{OutputDir: &siteDir, MaxConcurrent: &concurrent}.Then(Overlay{OutputDir: &stageDir})
	if merged.OutputDir == nil || *merged.OutputDir != stageDir {
		t.Fatalf("阶段那层应当赢：%+v", merged.OutputDir)
	}
	if merged.MaxConcurrent == nil || *merged.MaxConcurrent != concurrent {
		t.Fatalf("阶段没写的项应当保留站点那层的值：%+v", merged.MaxConcurrent)
	}
	if !(Overlay{}).Then(Overlay{}).IsZero() {
		t.Fatal("两层都空，合并结果应当还是空")
	}
}

func TestOverlayFields(t *testing.T) {
	if got := (Overlay{}).Fields(); len(got) != 0 {
		t.Fatalf("空覆盖层不该列出任何项：%v", got)
	}
	dir := "d"
	resume := false
	got := strings.Join(Overlay{OutputDir: &dir, EnableResume: &resume}.Fields(), ",")
	for _, want := range []string{"output_dir=d", "enable_resume=false"} {
		if !strings.Contains(got, want) {
			t.Errorf("Fields() 缺少 %q：%s", want, got)
		}
	}
	if strings.Contains(got, "chunk_size") {
		t.Errorf("没改的项不该出现：%s", got)
	}
}

// With：配置独立、状态共享 —— 和 m3u8 那份同一个约定（共享状态里那个 execMu 才是
// "同一目标文件不并发执行"的落点）。
func TestWithSharesStateAndIsolatesConfig(t *testing.T) {
	base := NewDownloader(DefaultConfig())
	concurrent := 2
	fork := base.With(Overlay{MaxConcurrent: &concurrent})

	if fork.st != base.st {
		t.Fatal("With 出来的实例应当共用内部状态（labors/代理连接池/消息队列）")
	}
	if fork.Config().MaxConcurrent != 2 {
		t.Fatalf("fork 的配置应当被覆盖，实得 %d", fork.Config().MaxConcurrent)
	}
	if got := base.Config().MaxConcurrent; got != DefaultConfig().MaxConcurrent {
		t.Fatalf("原实例的配置不该被动过，实得 %d", got)
	}
	base.st.laborMu.Lock()
	base.st.labors["k"] = &labor{url: "u", outputDir: "d", filename: "f"}
	base.st.laborMu.Unlock()
	fork.st.laborMu.RLock()
	_, ok := fork.st.labors["k"]
	fork.st.laborMu.RUnlock()
	if !ok {
		t.Fatal("fork 看不到原实例的 labor —— 那样两条并发下载就会各写各的同一个文件")
	}
	if NewDownloader(DefaultConfig()).st == base.st {
		t.Fatal("NewDownloader 应当是互不共享的一份（要共享用 With）")
	}
}
