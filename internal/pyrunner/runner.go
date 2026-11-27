// Package pyrunner runs Python bindings in an isolated native process.
package pyrunner

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

//go:embed worker.py
var worker string

type Call func(context.Context, string, map[string]any) (any, error)

const limit = 1 << 20

func Run(parent context.Context, script string, args map[string]any, check bool, call Call) (any, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	py, err := discover(ctx)
	if err != nil {
		return nil, err
	}
	return run(ctx, py, cancel, script, args, check, call)
}

func run(ctx context.Context, py interpreter, cancel context.CancelFunc, script string, args map[string]any, check bool, call Call) (any, error) {
	cmd, err := command(ctx, py)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	var diagnostic startupDiagnostic
	cmd.Stderr = &diagnostic
	in, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	if e = cmd.Start(); e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("isolated Python unavailable: %w", e)
	}
	waited := false
	defer func() {
		cancel()
		in.Close()
		if !waited {
			cmd.Wait()
		}
	}()
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
	in.Close()
	waitErr := cmd.Wait()
	waited = true
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if text := strings.TrimSpace(string(diagnostic.data[:diagnostic.n])); text != "" {
		return nil, fmt.Errorf("isolated Python worker (%s): %s", py.Executable, text)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("isolated Python worker (%s): %w", py.Executable, waitErr)
	}
	return nil, fmt.Errorf("isolated Python worker exited without a response (%s)", py.Executable)
}

// The supervisor may fail before it can emit a protocol frame. Keep a bounded
// prefix; Wait joins os/exec's writer before the diagnostic is consumed.
type startupDiagnostic struct {
	data [1024]byte
	n    int
}

func (d *startupDiagnostic) Write(p []byte) (int, error) {
	d.n += copy(d.data[d.n:], p)
	return len(p), nil
}
