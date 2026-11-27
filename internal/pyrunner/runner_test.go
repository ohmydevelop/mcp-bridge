package pyrunner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Run(t.Context(), "def run(args,ctx): return 1", nil, true, nil); err == nil {
		t.Fatal("preflight accepted missing isolation")
	}
}
func TestIsolatedWorker(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap not installed")
	}
	t.Setenv("BRIDGE_CRED_TEST", "private")
	marker, err := os.CreateTemp(t.TempDir(), "private")
	if err != nil {
		t.Fatal(err)
	}
	marker.Close()
	script := "def run(args,ctx):\n import os,socket\n assert 'BRIDGE_CRED_TEST' not in os.environ and not os.path.exists(args['private'])\n try: socket.socket()\n except PermissionError: pass\n else: raise AssertionError('network socket allowed')\n return ctx.call_tool('qa/test',{'x':1})"
	val, err := Run(t.Context(), script, map[string]any{"private": marker.Name()}, false, func(ctx context.Context, ref string, args map[string]any) (any, error) {
		if ref != "qa/test" || args["x"] != float64(1) {
			t.Fatal(ref, args)
		}
		return "pass", nil
	})
	if err != nil || val != "pass" {
		t.Fatal(val, err)
	}
	for _, src := range []string{
		"def run(args,ctx): return 'x'*(1<<20)",
		"def run(args,ctx): return bytearray(300<<20)",
		"def run(args,ctx):\n for i in range(51):ctx.call_tool('qa/test',{})",
	} {
		_, err := Run(t.Context(), src, nil, false, func(context.Context, string, map[string]any) (any, error) { return nil, nil })
		if err == nil {
			t.Fatal("limit did not fire", src)
		}
	}
	if _, err := Run(t.Context(), "raise Exception('ran at publish')\ndef run(args,ctx): return 1", nil, true, nil); err != nil {
		t.Fatal(err)
	}
	for _, cancelEarly := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		if cancelEarly {
			time.AfterFunc(75*time.Millisecond, cancel)
		}
		_, err := Run(ctx, "def run(args,ctx):\n while True: pass", nil, false, nil)
		cancel()
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
}
