package pyrunner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

type interpreter struct {
	Executable string `json:"executable"`
	Stdlib     string `json:"stdlib"`
	Platstdlib string `json:"platstdlib"`
	Library    string `json:"library"`
	Framework  string `json:"framework"`
}

func PythonPath(ctx context.Context) (string, error) {
	py, err := discover(ctx)
	return py.Executable, err
}

// Probe starts the same worker used for execution and checks the callback and host boundaries.
func Probe(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	py, err := discover(ctx)
	if err != nil {
		return "", err
	}
	private, err := os.CreateTemp("", "bridge-runner-private-")
	if err != nil {
		return py.Executable, err
	}
	defer os.Remove(private.Name())
	if err := private.Close(); err != nil {
		return py.Executable, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return py.Executable, err
	}
	defer listener.Close()
	const script = `def run(args, ctx):
    import os, socket, sys
    try:
        open(args["marker"], "rb").close()
    except (PermissionError, FileNotFoundError):
        pass
    else:
        raise RuntimeError("worker can read private host files")
    try:
        with socket.create_connection(("127.0.0.1", args["port"]), timeout=1):
            pass
    except OSError:
        pass
    else:
        raise RuntimeError("worker can connect to host network")
    if ctx.call_tool("probe/echo", {}) != "ok":
        raise RuntimeError("worker callback failed")
    return sys.version.split()[0]`
	version, err := run(ctx, py, cancel, script, map[string]any{"marker": private.Name(), "port": listener.Addr().(*net.TCPAddr).Port}, false, func(_ context.Context, ref string, _ map[string]any) (any, error) {
		if ref != "probe/echo" {
			return nil, errors.New("invalid probe callback")
		}
		return "ok", nil
	})
	if err != nil {
		return py.Executable, err
	}
	return fmt.Sprintf("%s (Python %v)", py.Executable, version), nil
}
