package pyrunner

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUnavailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux isolation requires bubblewrap on PATH")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := Run(t.Context(), "def run(args,ctx): return 1", nil, true, nil); err == nil {
		t.Fatal("preflight accepted missing isolation")
	}
}
func TestIsolatedWorker(t *testing.T) {
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bubblewrap not installed")
		}
	}
	path, err := Probe(t.Context())
	if err != nil || !strings.Contains(path, "(Python 3.") {
		t.Fatalf("runner self-check: %s, %v", path, err)
	}
	t.Setenv("BRIDGE_CRED_TEST", "private")
	script := "def run(args,ctx):\n import os, datetime, html, _socket\n from collections import Counter\n assert datetime.datetime(2026, 1, 1).year == 2026 and html.escape('<') == '&lt;' and Counter('aa')['a'] == 2\n assert 'BRIDGE_CRED_TEST' not in os.environ\n return ctx.call_tool('qa/test',{'x':1})"
	val, err := Run(t.Context(), script, nil, false, func(ctx context.Context, ref string, args map[string]any) (any, error) {
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
		"def run(args,ctx):\n x=bytearray(300<<20)\n return len(x)",
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
