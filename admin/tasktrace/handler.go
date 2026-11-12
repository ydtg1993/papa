// Package tasktrace serves task timelines and archived task pages.
package tasktrace

import (
	"encoding/json"
	"fmt"
	"github.com/ydtg1993/papa/v4/core"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Logger interface{ Errorf(string, ...any) }

// Options supplies the trace lookup and archive root, without execution state.
type Options struct {
	ArchiveDir string
	Trace      func(int) ([]core.TraceStep, error)
	Logger     Logger
}
type Handler struct {
	archiveDir string
	trace      func(int) ([]core.TraceStep, error)
	logger     Logger
}

func New(o Options) *Handler {
	return &Handler{archiveDir: o.ArchiveDir, trace: o.Trace, logger: o.Logger}
}

// taskTraceHandler 返回一条任务的步骤追踪时间线（按尝试、步骤排序）。
func (s *Handler) Trace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.trace == nil {
		http.Error(w, "task trace not configured", http.StatusNotFound)
		return
	}
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil || id <= 0 {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	steps, err := s.trace(id)
	if err != nil {
		// 业务原因（如追踪开关没开）原样回给前端，抽屉里直接显示这句话
		s.logger.Errorf("load task trace %d: %s", id, err.Error())
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"task_id": id, "steps": steps})
}

// taskPageHandler 下载一个归档页面（`GET /api/task/page?file=<相对路径>`）。
//
// 给的是**文件本身**（`Content-Disposition: attachment`）而不是内联渲染 —— 拿下去本地分析
// （对着真实页面写选择器）比在浏览器里看更实用，也省掉"注入 base 标签才能长回原样"那套。
func (s *Handler) Page(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.archiveDir == "" {
		http.Error(w, "页面归档未开启（crawler.archive.enabled）", http.StatusNotFound)
		return
	}
	rel := r.URL.Query().Get("file")
	abs, err := ResolveArchivedFile(s.archiveDir, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// 字符串校验只处理请求格式；实际打开必须由根目录句柄约束，
	// 防止父目录/最终文件的链接以及检查后替换把读取带出归档目录。
	root, err := os.OpenRoot(s.archiveDir)
	if err != nil {
		http.Error(w, "归档文件读不到（可能已过保留期）", http.StatusNotFound)
		return
	}
	defer root.Close()
	inside, err := filepath.Rel(s.archiveDir, abs)
	if err != nil {
		http.Error(w, "file 参数越界", http.StatusBadRequest)
		return
	}
	f, err := root.Open(inside)
	if err != nil {
		http.Error(w, "归档文件读不到（可能已过保留期）", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "归档文件读不到（可能已过保留期）", http.StatusNotFound)
		return
	}
	b, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "归档文件读不到（可能已过保留期）", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(abs)+"\"")
	_, _ = w.Write(b)
}

// ResolveArchivedFile 把一个**相对路径**解析到归档根目录下的绝对路径；越界一律拒绝。
//
// 两道判据：先按路径语义清洗（拒绝绝对路径与 `..`），再确认解析结果确实落在根之下
// （防大小写/分隔符之类的意外）。归档目录里的文件名由框架生成，但路径是**从请求里来的** ——
// 那是不可信输入，这里不靠"文件名不含 .."这种假设。
func ResolveArchivedFile(root, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("缺少 file 参数")
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("file 参数非法")
	}
	// 明确拒掉"看起来就是绝对路径"的输入：Windows 上 `filepath.IsAbs("\etc\passwd")` 是 **false**
	//（没有盘符不算绝对路径），光靠 IsAbs 会把它当相对路径拼进根目录 —— 拼进去虽然仍在根下，
	// 但那种输入本来就不该被接受。带盘符的（`C:…`）同理。
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") ||
		(len(rel) >= 2 && rel[1] == ':') {
		return "", fmt.Errorf("file 参数越界")
	}
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	// `example/..` 清洗完是 `.`（指到根目录本身）—— 那不是文件，也别让它再往下走。
	if cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file 参数越界")
	}
	abs := filepath.Join(root, cleaned)
	inside, err := filepath.Rel(root, abs)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file 参数越界")
	}
	return abs, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
