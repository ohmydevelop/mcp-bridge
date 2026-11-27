//go:build darwin || windows

package pyrunner

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestNativeCandidates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "运行 Python")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_PYTHON", "")
	t.Setenv("BRIDGE_PYTHON_HINT_FILE", "")
	t.Setenv("PATH", dir)
	if _, err := resolve(t.Context(), pathCandidates()); err == nil {
		t.Fatal("accepted an empty PATH")
	}
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = append(names, "py")
	}
	for _, name := range names {
		filename := filepath.Join(dir, name)
		if runtime.GOOS == "windows" {
			filename += ".exe"
		}
		if err := os.WriteFile(filename, []byte("invalid executable"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"python", "python3"}
	if runtime.GOOS == "windows" {
		want = append(want, "py")
	}
	found := pathCandidates()
	if len(found) != len(want) {
		t.Fatalf("candidates: %v", found)
	}
	for i, name := range want {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if found[i][0] != filepath.Join(dir, name) {
			t.Fatalf("candidate %v, want %s", found[i], name)
		}
	}
	if runtime.GOOS == "windows" && !slices.Equal(found[2][1:], []string{"-3"}) {
		t.Fatalf("launcher: %v", found[2])
	}
	t.Setenv("BRIDGE_PYTHON", filepath.Join(dir, "installed-python"))
	if got := pathCandidates()[0][0]; got != os.Getenv("BRIDGE_PYTHON") {
		t.Fatal("install hint ignored", got)
	}
	if _, err := resolve(t.Context(), found); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid executable accepted or missing", err)
	}
}

func TestInstalledHintRefresh(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "python-path")
	t.Setenv("BRIDGE_PYTHON_HINT_FILE", file)
	t.Setenv("BRIDGE_PYTHON", filepath.Join(dir, "fallback"))
	hints := []struct{ value, want string }{
		{"relative", os.Getenv("BRIDGE_PYTHON")},
		{"\uFEFF" + filepath.Join(dir, "新 python") + "\r\n", filepath.Join(dir, "新 python")},
		{filepath.Join(dir, "newer") + "\n", filepath.Join(dir, "newer")},
		{filepath.Join(dir, "bad") + "\nsecond line", os.Getenv("BRIDGE_PYTHON")},
	}
	for _, hint := range hints {
		if err := os.WriteFile(file, []byte(hint.value), 0600); err != nil {
			t.Fatal(err)
		}
		if got := pathCandidates()[0][0]; got != hint.want {
			t.Fatalf("got %q, want %q", got, hint.want)
		}
	}
}
