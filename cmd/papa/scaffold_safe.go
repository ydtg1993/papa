package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Refuse linked ancestors: missing-only must not seed files outside the named
// project through a preexisting symlink/junction. Exclusive create also protects
// existing files if another scaffold process reaches the same path first.
func checkScaffoldPath(path string) error {
	abs, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	for p := abs; ; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		// Go reports Windows junctions and other unsupported reparse points as
		// ModeIrregular, rather than ModeSymlink. Neither is a safe scaffold path.
		if e == nil && info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("脚手架路径包含链接，拒绝写入：%s", p)
		}
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
	}
	return nil
}
func writeScaffoldFile(path string, data []byte, missingOnly bool) error {
	if e := checkScaffoldPath(path); e != nil {
		return e
	}
	if info, e := os.Lstat(path); e == nil {
		if missingOnly && info.Mode().IsRegular() {
			return nil
		}
		return fmt.Errorf("脚手架目标已存在，拒绝覆盖：%s", path)
	} else if !os.IsNotExist(e) {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if e != nil {
		if missingOnly && os.IsExist(e) {
			info, statErr := os.Lstat(path)
			if statErr == nil && info.Mode().IsRegular() {
				return nil
			}
		}
		return e
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// replaceScaffoldFile refreshes one framework-owned name without writing through
// a hard link to project-owned content. Keep the old file until the new contents
// have been written and closed. Ancestor checks are not a sandbox against a
// concurrent process replacing directories; callers own a trusted project tree.
func replaceScaffoldFile(path string, data []byte) error {
	if err := checkScaffoldPath(path); err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("脚手架目标不是普通文件，拒绝刷新：%s", path)
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".papa-refresh-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // only this call's temporary file
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Chmod(mode)
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := checkScaffoldPath(path); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
