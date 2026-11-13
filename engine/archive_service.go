package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/google/uuid"
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
	return &archiveBuffer{executionID: uuid.NewString(), store: s, logger: s.logger, dir: s.options.Dir, mode: s.mode(), maxBytes: s.maxBytes(), task: task, attempt: attempt, byURL: make(map[string]int)}
}

// writeOne 写一页：正文按**原样字节**落 .html，旁边落一份 .json 记这次请求的元信息。
func (s archiveService) writeOne(task *Task, attempt int, executionID string, page archivedPage) (string, error) {
	root := s.options.Dir
	rel := archiveRelativePath(task, attempt, executionID, page)
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
		"execution_id": executionID,
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

// archiveRelativePath separates every claim and retry, including restart recovery.
func archiveRelativePath(task *Task, attempt int, executionID string, page archivedPage) string {
	sum := sha256.Sum256([]byte(page.url))
	name := fmt.Sprintf("task-%d-run-%s-try-%d-%s.html", task.ID, executionID, attempt, hex.EncodeToString(sum[:4]))
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
