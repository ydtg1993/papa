package eventlog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupTimeFormat 是 lumberjack 给轮转备份命名时使用的时间戳格式，
// 与 lumberjack 源码中的 backupTimeFormat 保持一致。
const backupTimeFormat = "2006-01-02T15-04-05.000"

// Discover 由主文件路径推出 lumberjack 轮转备份的路径列表。
//
// lumberjack 的备份命名规则（<base> 为主文件名，<ext> 为其扩展名）：
//
//	<base>-<timestamp><ext>      未压缩备份
//	<base>-<timestamp><ext>.gz   压缩备份（compress: true 时）
//
// 返回值包含主文件（activePath 存在时）与本目录下可识别的全部备份，按 ModTime
// 升序排列（同一修改时刻按路径升序），便于调用方按写入先后读取。
// 主文件不存在不算错误；目录不存在或没有匹配文件时返回空列表与 nil。
// 目录与文件名匹配但时间戳不是 lumberjack 格式的文件不会被当成备份。
func Discover(activePath string) ([]string, error) {
	if strings.TrimSpace(activePath) == "" {
		return nil, errors.New("eventlog: empty active path")
	}
	activePath = filepath.Clean(activePath)
	dir := filepath.Dir(activePath)
	base := filepath.Base(activePath)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("eventlog: read event dir %q: %w", dir, err)
	}

	type foundFile struct {
		path string
		mod  time.Time
	}
	found := make([]foundFile, 0, len(entries))
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if name != base && !isBackupName(name, prefix, ext) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			return nil, fmt.Errorf("eventlog: stat %q: %w", filepath.Join(dir, name), err)
		}
		found = append(found, foundFile{path: filepath.Join(dir, name), mod: info.ModTime()})
	}
	sort.Slice(found, func(i, j int) bool {
		if !found[i].mod.Equal(found[j].mod) {
			return found[i].mod.Before(found[j].mod)
		}
		return found[i].path < found[j].path
	})

	paths := make([]string, 0, len(found))
	for _, f := range found {
		paths = append(paths, f.path)
	}
	return paths, nil
}

// isBackupName 判断 name 是否是 lumberjack 为主文件（prefix + ext）生成的备份名。
// 中间的时间戳按 backupTimeFormat 校验，避免把名字碰巧相似的文件误认成备份。
func isBackupName(name, prefix, ext string) bool {
	rest, ok := strings.CutPrefix(name, prefix+"-")
	if !ok || rest == "" {
		return false
	}
	var stamp string
	switch {
	case ext == "":
		// 主文件没有扩展名时，压缩备份是 <base>-<ts>.gz，未压缩备份是 <base>-<ts>。
		if s, ok := strings.CutSuffix(rest, ".gz"); ok {
			stamp = s
		} else {
			stamp = rest
		}
	case strings.HasSuffix(rest, ext+".gz"):
		stamp = strings.TrimSuffix(rest, ext+".gz")
	case strings.HasSuffix(rest, ext):
		stamp = strings.TrimSuffix(rest, ext)
	default:
		return false
	}
	if stamp == "" {
		return false
	}
	_, err := time.Parse(backupTimeFormat, stamp)
	return err == nil
}
