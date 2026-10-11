package engine

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
)

// 归档的几个取配置的口子：没配时要有**明确默认**，显式负数表示"永久保留"。
// 这几个函数以前 0% 覆盖 —— 默认值写错（比如负数被当成"用默认"）只表现为"归档文件悄悄被删了"。
func TestArchiveConfigDefaults(t *testing.T) {
	e := stageDepEngine(t)

	// 关着：不给目录（后台那条"下载这一页"据此判断有没有归档可看）
	if got := e.ArchiveDir(); got != "" {
		t.Fatalf("归档没开时 ArchiveDir 应当是空串，实得 %q", got)
	}
	// 未配置 → failure；配了 always → always
	if got := e.archiveMode(); got != config.ArchiveModeFailure {
		t.Fatalf("未配置时生效 mode = %q, want failure", got)
	}
	e.cfg.Crawler.Archive.Mode = config.ArchiveModeAlways
	if got := e.archiveMode(); got != config.ArchiveModeAlways {
		t.Fatalf("配了 always 时生效 mode = %q", got)
	}
	// 保留期：未配置 → 默认；负数 → 0（= 不自动清理）；正数 → 原值
	if got := e.archiveRetention(); got != defaultArchiveRetention {
		t.Fatalf("未配置时保留期 = %v, want %v", got, defaultArchiveRetention)
	}
	e.cfg.Crawler.Archive.Retention = -time.Hour
	if got := e.archiveRetention(); got != 0 {
		t.Fatalf("显式负数（永久保留）应当算成 0，实得 %v", got)
	}
	e.cfg.Crawler.Archive.Retention = 3 * time.Hour
	if got := e.archiveRetention(); got != 3*time.Hour {
		t.Fatalf("配了保留期就该用它，实得 %v", got)
	}
	// 单页上限：未配置 → 默认；配了 MB → 换算成字节
	if got := e.archiveMaxBytes(); got != defaultArchiveMaxBytes {
		t.Fatalf("未配置时上限 = %d, want %d", got, defaultArchiveMaxBytes)
	}
	e.cfg.Crawler.Archive.MaxFileMB = 2
	if got := e.archiveMaxBytes(); got != 2<<20 {
		t.Fatalf("配 2MB 时上限 = %d, want %d", got, int64(2)<<20)
	}

	// 开着归档时 ArchiveDir 要给目录
	e.cfg.Crawler.Archive.Enabled = true
	e.cfg.Crawler.Archive.Dir = "./logs/fetcher-html"
	if got := e.ArchiveDir(); got != "./logs/fetcher-html" {
		t.Fatalf("归档开着时 ArchiveDir = %q", got)
	}
}
