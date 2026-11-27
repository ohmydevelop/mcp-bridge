// Package pyrunner runs Python bindings in an isolated Linux process.
package pyrunner

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

//go:embed worker.py
var worker string

type Call func(context.Context, string, map[string]any) (any, error)

const limit = 1 << 20

func Run(parent context.Context, script string, args map[string]any, check bool, call Call) (any, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	argv := []string{"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--ro-bind", "/usr/lib", "/usr/lib", "--ro-bind", "/usr/bin/python3", "/usr/bin/python3"}
	for _, dir := range []string{"/lib", "/lib64"} {
		if _, e := os.Stat(dir); e == nil {
			argv = append(argv, "--ro-bind", dir, dir)
		}
	}
	argv = append(argv, "--size", "8388608", "--tmpfs", "/tmp", "--remount-ro", "/", "--chdir", "/tmp", "--", "/usr/bin/python3", "-ISB", "-c", worker)
	cmd := exec.CommandContext(ctx, "bwrap", argv...)
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	in, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	if e = cmd.Start(); e != nil {
		return nil, fmt.Errorf("isolated Python unavailable: %w", e)
	}
	defer func() { cancel(); in.Close(); cmd.Wait() }()
	enc := json.NewEncoder(in)
	if e = enc.Encode(map[string]any{"script": script, "args": args, "check": check}); e != nil {
		return nil, e
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), limit+1024)
	calls := 0
	for scanner.Scan() {
		var frame struct {
			Op, Tool, Error string
			Arguments       map[string]any
			Value           json.RawMessage
		}
		if e = json.Unmarshal(scanner.Bytes(), &frame); e != nil {
			return nil, errors.New("invalid Python protocol frame")
		}
		switch frame.Op {
		case "result":
			if len(frame.Value) > limit {
				return nil, errors.New("Python result exceeds 1 MiB")
			}
			var value any
			e = json.Unmarshal(frame.Value, &value)
			return value, e
		case "error":
			return nil, fmt.Errorf("Python: %s", frame.Error)
		case "call":
			calls++
			if check || call == nil || calls > 50 {
				return nil, errors.New("Python call limit or preflight restriction")
			}
			v, e := call(ctx, frame.Tool, frame.Arguments)
			if e != nil {
				return nil, e
			}
			if e = enc.Encode(map[string]any{"value": v}); e != nil {
				return nil, e
			}
		default:
			return nil, errors.New("invalid Python operation")
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if scanner.Err() != nil {
		return nil, errors.New("Python output exceeds limit")
	}
	return nil, errors.New("isolated Python unavailable or worker exited; check bubblewrap, namespaces, Python and libseccomp")
}
