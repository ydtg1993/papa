package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const logTailLines = 200
const logTailBytes = 1 << 20

type logTail struct {
	Name      string `json:"name"`
	Text      string `json:"text"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
}

func (s *Monitor) logsTailHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", 405)
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00:") {
		http.Error(w, "无效日志文件名", 400)
		return
	}
	if s.cfg.LogDir == "" {
		http.Error(w, "未配置日志目录", 404)
		return
	}
	root, err := os.OpenRoot(s.cfg.LogDir)
	if err != nil {
		http.Error(w, "日志目录不可用", 404)
		return
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		http.Error(w, "文件不存在或已轮转，请刷新列表", 404)
		return
	}
	if !info.Mode().IsRegular() {
		http.Error(w, "仅支持普通日志文件", 400)
		return
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".gz", ".zip", ".bz2", ".xz":
		http.Error(w, "压缩日志请使用全部打包下载", 400)
		return
	}
	f, err := root.Open(name)
	if err != nil {
		http.Error(w, "无法读取日志文件", 404)
		return
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		http.Error(w, "文件已变化，请刷新重试", 409)
		return
	}
	tail, err := readLogTail(f, current.Size())
	if err != nil {
		http.Error(w, "日志读取失败或文件正在轮转，请重试", 409)
		return
	}
	tail.Name = name
	writeJSON(w, tail)
}

// Read backwards from a size snapshot: appends do not extend this request.
func readLogTail(r io.ReaderAt, size int64) (logTail, error) {
	var result logTail
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
