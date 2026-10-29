// Package fsutil holds the file-walking rules shared by every package that
// scans a Terraform project directory.
package fsutil

import (
	"os"
	"path/filepath"
	"strings"
)

// skipDirs are tool caches and metadata directories that never contain
// generated project files (.terraform holds third-party module/provider code).
var skipDirs = map[string]bool{
	".terraform":    true,
	".git":          true,
	".tf-architect": true,
}

// WalkFiles calls fn for every regular file under root, skipping skipDirs.
// A missing root is not an error.
func WalkFiles(root string, fn func(path string, info os.FileInfo) error) error {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if path != root && skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return fn(path, info)
	})
}

// TFFiles returns the paths of all .tf files under root.
func TFFiles(root string) ([]string, error) {
	var files []string
	err := WalkFiles(root, func(path string, _ os.FileInfo) error {
		if strings.HasSuffix(path, ".tf") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}
