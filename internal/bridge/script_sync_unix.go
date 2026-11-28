//go:build !windows

package bridge

import (
	"os"
	"path/filepath"
)

func syncScriptDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func publishScriptFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return syncScriptDir(filepath.Dir(to))
}
