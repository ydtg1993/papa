package main

import "path/filepath"

// frameworkDocsDir is the only framework-owned documentation location in v4.
func frameworkDocsDir(root string) string {
	return filepath.Join(root, "docs", "framework")
}
