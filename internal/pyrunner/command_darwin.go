package pyrunner

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

//go:embed sandbox_darwin.py
var sandboxBootstrap string

func systemCandidates() [][]string {
	// launchd does not load the user's shell startup files.
	return [][]string{{"/opt/homebrew/bin/python3"}, {"/usr/local/bin/python3"}}
}

func command(ctx context.Context, py interpreter) (*exec.Cmd, error) {
	// The fixed bootstrap installs the OS policy before the worker reads stdin.
	args := []string{"-ISB", "-X", "utf8", "-c", sandboxBootstrap + "\n" + worker}
	for _, path := range []string{py.Stdlib, py.Platstdlib} {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(filepath.Base(real), "python3.") || filepath.Base(filepath.Dir(real)) != "lib" {
			return nil, fmt.Errorf("unexpected Python standard library directory: %s", real)
		}
		args = append(args, real)
	}
	for _, lib := range []string{py.Executable, py.Library, py.Framework} {
		if lib == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(lib)
		if err != nil {
			continue
		}
		if info, err := os.Stat(real); err == nil && info.Mode().IsRegular() {
			args = append(args, real)
		}
	}
	cmd := exec.CommandContext(ctx, py.Executable, args...)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd, nil
}
