package main

import "path/filepath"

// siteDirectory prefers application code under internal while accepting older scaffolds.
func siteDirectory(root string) string {
	if exists(filepath.Join(root, "internal", "sites")) {
		return filepath.Join("internal", "sites")
	}
	return "sites"
}

func frameworkDocsDir(root string) string {
	dir := filepath.Join(root, "docs", "framework")
	if exists(dir) {
		return dir
	}
	return filepath.Join(root, "docs")
}
