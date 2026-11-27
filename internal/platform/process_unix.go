//go:build linux || darwin

package platform

import (
	"context"
	"os/exec"
	"syscall"
)

func ProcessHelper() (bool, int) { return false, 0 }
func CLICommand(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd, nil
}
