package logs

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Only log files and numeric rotations belong in the viewer/export.
func isLogFileName(name string) bool {
	name = strings.ToLower(name)
	if strings.HasPrefix(name, ".") {
		return false
	}
	name = strings.TrimSuffix(name, ".gz")
	if strings.HasSuffix(name, ".log") {
		return true
	}
	if i := strings.LastIndex(name, ".log."); i >= 0 {
		n, err := strconv.Atoi(name[i+5:])
		return err == nil && n >= 0
	}
	return false
}

const logTailLines = 200
const logTailBytes = 1 << 20

type TailResult struct {
	Name      string `json:"name"`
	Text      string `json:"text"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
}

func (s *Handler) Tail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", 405)
		return
	}
	name := r.URL.Query().Get("file")
	f, info := s.openLogFile(w, name)
	if f == nil {
		return
	}
	defer f.Close()
	if strings.EqualFold(filepath.Ext(name), ".gz") {
		http.Error(w, "压缩日志请使用全部打包下载", 400)
		return
	}
	tail, err := readLogTail(f, info.Size())
	if err != nil {
		http.Error(w, "日志读取失败或文件正在轮转，请重试", 409)
		return
	}
	tail.Name = name
	writeJSON(w, tail)
}

// openLogFile 为查看和下载共用同一条安全边界。失败时已写 HTTP 错误；
// 成功时由调用方关闭文件。根目录约束和打开后的身份核对防止轮转/替换越界。
func (s *Handler) openLogFile(w http.ResponseWriter, name string) (*os.File, os.FileInfo) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00:") {
		http.Error(w, "无效日志文件名", 400)
		return nil, nil
	}
	if !isLogFileName(name) {
		http.Error(w, "仅支持日志文件", 400)
		return nil, nil
	}
	if s.dir == "" {
		http.Error(w, "未配置日志目录", 404)
		return nil, nil
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		http.Error(w, "日志目录不可用", 404)
		return nil, nil
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		http.Error(w, "文件不存在或已轮转，请刷新列表", 404)
		return nil, nil
	}
	if !info.Mode().IsRegular() {
		http.Error(w, "仅支持普通日志文件", 400)
		return nil, nil
	}
	f, err := root.Open(name)
	if err != nil {
		http.Error(w, "无法读取日志文件", 404)
		return nil, nil
	}
	current, err := f.Stat()
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		f.Close()
		http.Error(w, "文件已变化，请刷新重试", 409)
		return nil, nil
	}
	return f, current
}

// Read backwards from a size snapshot: appends do not extend this request.
func readLogTail(r io.ReaderAt, size int64) (TailResult, error) {
	var result TailResult
	if size < 0 {
		return result, fmt.Errorf("invalid file size")
	}
	pos := size
	data := []byte{}
	newlines := 0
	for pos > 0 && len(data) < logTailBytes && newlines <= logTailLines {
		n := int64(8192)
		if pos < n {
			n = pos
		}
		if int64(logTailBytes-len(data)) < n {
			n = int64(logTailBytes - len(data))
		}
		chunk := make([]byte, int(n))
		pos -= n
		count, err := r.ReadAt(chunk, pos)
		if err != nil && err != io.EOF {
			return result, err
		}
		if count != len(chunk) {
			return result, io.ErrUnexpectedEOF
		}
		newlines += bytes.Count(chunk, []byte{'\n'})
		data = append(chunk, data...)
	}
	if len(data) == 0 {
		return result, nil
	}
	ended := data[len(data)-1] == '\n'
	if ended {
		data = data[:len(data)-1]
	}
	parts := bytes.Split(data, []byte{'\n'})
	if len(parts) > logTailLines {
		parts = parts[len(parts)-logTailLines:]
	} else if pos > 0 {
		result.Truncated = true
	}
	body := bytes.Join(parts, []byte{'\n'})
	if result.Truncated {
		for len(body) > 0 && !utf8.RuneStart(body[0]) {
			body = body[1:]
		}
	}
	result.Lines = len(parts)
	if ended {
		body = append(body, '\n')
	}
	result.Text = strings.ToValidUTF8(strings.ReplaceAll(string(body), "\r\n", "\n"), "�")
	return result, nil
}

// Options supplies the log directory; the component owns file access policy.
type Options struct{ Dir string }
type Handler struct{ dir string }

func New(o Options) *Handler { return &Handler{dir: o.Dir} }

type logFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// logsListHandler 列出日志目录下的文件
func (s *Handler) List(w http.ResponseWriter, r *http.Request) {
	files := []logFile{}
	if s.dir != "" {
		if entries, err := os.ReadDir(s.dir); err == nil {
			for _, e := range entries {
				if !isLogFileName(e.Name()) || !e.Type().IsRegular() {
					continue
				}
				if info, err := e.Info(); err == nil {
					files = append(files, logFile{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
				}
			}
		}
	}
	writeJSON(w, map[string]any{"log_dir": s.dir, "files": files})
}

// logsDownloadHandler 下载单个日志文件（?file=name），缺省打包全部为 zip
func (s *Handler) Download(w http.ResponseWriter, r *http.Request) {
	dir := s.dir
	if dir == "" {
		http.Error(w, "log dir not configured", http.StatusNotFound)
		return
	}
	if name := r.URL.Query().Get("file"); name != "" {
		f, info := s.openLogFile(w, name)
		if f == nil {
			return
		}
		defer f.Close()
		http.ServeContent(w, r, name, info.ModTime(), f)
		return
	}
	zipLogs(w, dir)
}

// zipLogs 将日志目录下所有文件打包为 zip 响应
func zipLogs(w http.ResponseWriter, dir string) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="logs.zip"`)
	zw := zip.NewWriter(w)
	defer zw.Close()

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || !isLogFileName(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		fw, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return nil
		}
		_, _ = io.Copy(fw, src)
		_ = src.Close()
		return nil
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

const TailLines = logTailLines
const TailBytes = logTailBytes

func ReadTail(r io.ReaderAt, size int64) (TailResult, error) { return readLogTail(r, size) }
func IsFileName(name string) bool                            { return isLogFileName(name) }
