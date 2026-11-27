package pyrunner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
	"newmcp/mcp-bridge/internal/platform"
)

// Services retain a snapshot of PATH; pick up Python installed after Bridge started.
func systemCandidates() [][]string {
	var out [][]string
	for _, source := range []struct {
		root registry.Key
		key  string
	}{
		{registry.CURRENT_USER, `Environment`},
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`},
	} {
		key, err := registry.OpenKey(source.root, source.key, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		path, _, err := key.GetStringValue("Path")
		key.Close()
		if err != nil {
			continue
		}
		for _, dir := range filepath.SplitList(path) {
			dir, err = registry.ExpandString(strings.TrimSpace(dir))
			if err != nil || !filepath.IsAbs(dir) {
				continue
			}
			for _, name := range []string{"python.exe", "python3.exe", "py.exe"} {
				file := filepath.Join(dir, name)
				if info, err := os.Stat(file); err != nil || info.IsDir() {
					continue
				}
				candidate := []string{file}
				if name == "py.exe" {
					candidate = append(candidate, "-3")
				}
				out = append(out, candidate)
			}
		}
	}
	return out
}
func command(ctx context.Context, py interpreter) (*exec.Cmd, error) {
	return platform.SandboxCommand(ctx, py.Executable, "-ISB", "-X", "utf8", "-c", worker)
}
