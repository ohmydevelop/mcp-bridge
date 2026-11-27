package pyrunner

import (
	"context"
	_ "embed"
	"os"
	"os/exec"
	"syscall"
)

//go:embed limit_linux.py
var limits string

// Keep the Linux definition bound to the interpreter installed in the bubblewrap root.
func discover(ctx context.Context) (interpreter, error) {
	return interpreter{Executable: "/usr/bin/python3"}, nil
}
func command(ctx context.Context, py interpreter) (*exec.Cmd, error) {
	argv := []string{"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--ro-bind", "/usr/lib", "/usr/lib", "--ro-bind", "/usr/bin/python3", "/usr/bin/python3"}
	for _, dir := range []string{"/lib", "/lib64"} {
		if _, e := os.Stat(dir); e == nil {
			argv = append(argv, "--ro-bind", dir, dir)
		}
	}
	argv = append(argv, "--size", "8388608", "--tmpfs", "/tmp", "--remount-ro", "/", "--chdir", "/tmp", "--", "/usr/bin/python3", "-ISB", "-c", limits+"\n"+worker)
	cmd := exec.CommandContext(ctx, "bwrap", argv...)
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd, nil
}
