package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/config"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// archiveService owns policy and filesystem operations, while archiveBuffer
// owns attempt pages and Engine owns maintenance cancellation and drain.
type archiveService struct {
	options config.ArchiveConfig
	logger  *logrus.Logger
}

func (s archiveService) enabled() bool { return s.options.Enabled && s.options.Dir != "" }
func (s archiveService) mode() string {
	if s.options.Mode == config.ArchiveModeAlways {
		return config.ArchiveModeAlways
	}
	return config.ArchiveModeFailure
}
func (s archiveService) retention() time.Duration {
	if s.options.Retention < 0 {
		return 0
	}
	if s.options.Retention == 0 {
		return defaultArchiveRetention
	}
	return s.options.Retention
}
func (s archiveService) maxBytes() int64 {
	if s.options.MaxFileMB > 0 {
		return int64(s.options.MaxFileMB) << 20
	}
	return defaultArchiveMaxBytes
}
func (s archiveService) newBuffer(task *Task, attempt int) *archiveBuffer {
	if !s.enabled() || task.ID == 0 {
		return nil
	}
	return &archiveBuffer{store: s, logger: s.logger, dir: s.options.Dir, mode: s.mode(), maxBytes: s.maxBytes(), task: task, attempt: attempt, byURL: make(map[string]int)}
}

// writeOne 写一页：正文按**原样字节**落 .html，旁边落一份 .json 记这次请求的元信息。
func (s archiveService) writeOne(task *Task, attempt int, page archivedPage) (string, error) {
	root := s.options.Dir
	rel := archiveRelativePath(task, page)
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	// 原样字节：page.HTML 就是 htmlfetch 读到的 body，不做任何再序列化
	if err := os.WriteFile(abs, []byte(page.html), 0o644); err != nil {
		return "", err
	}

	meta := map[string]any{
		"task_id":      task.ID,
		"stage":        task.Stage,
		"site":         task.Site,
		"attempt":      attempt,
		"retry":        task.Retry,
		"task_url":     task.URL,
		"final_url":    page.url,
		"status":       page.status,
		"content_type": page.contentType,
		"bytes":        len(page.html),
		"archived_at":  time.Now().Format(time.RFC3339),
	}
	if b, err := marshalNoHTMLEscape(meta); err == nil {
		// 元信息写不进去不算失败：正文才是证据。文件名把 .html 换成 .json（与正文同名同目录）
		_ = os.WriteFile(filepath.Join(root, strings.TrimSuffix(rel, ".html")+".json"), b, 0o644)
	}
	return rel, nil
}

// relPath 归档文件的相对路径：`{site}/{stage}/task-{id}-try-{retry}-{urlhash8}.html`。
//
// **按站点分一层**：阶段名只在本站内唯一，两个站各有 `catalog` 时会写进同一个目录、混在一起
// （文件名带全局唯一的 task ID，不会互相覆盖，只是"这个阶段抓到过哪些页"再也分不开）。
// 站点为空只在旧行上还会遇到（新任务必须有 site），那一段落成 `_`，见 sanitizePathSegment。
//
// 带 try（= task.Retry）是必须的：同一任务重试 3 次时后面那次会覆盖前面那次，
// 而"第一次是登录墙、第三次正常"恰好是排查的关键。urlhash8 保证一个尝试里的多页不打架。
func archiveRelativePath(task *Task, page archivedPage) string {
	sum := sha256.Sum256([]byte(page.url))
	name := fmt.Sprintf("task-%d-try-%d-%s.html", task.ID, task.Retry, hex.EncodeToString(sum[:4]))
	return filepath.Join(sanitizePathSegment(task.Site), sanitizePathSegment(task.Stage), name)
}

func (s archiveService) cleanup(ctx context.Context, retention time.Duration) int {
	root := s.options.Dir
	cutoff := time.Now().Add(-retention)
	var files int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fs.SkipAll
		default:
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || !info.ModTime().Before(cutoff) {
			return nil
		}
		if rerr := os.Remove(path); rerr != nil {
			_ = rerr // 删不掉就留到下一轮（可能正被人打开着）
			return nil
		}
		files++
		return nil
	})
	if err != nil {
		s.logger.Warnf("archive cleanup: %s", err.Error())
	}
	if files > 0 {
		s.logger.Infof("archive cleanup: 删除 %d 个超过保留期（%s）的归档文件", files, retention)
	}
	return files
}
