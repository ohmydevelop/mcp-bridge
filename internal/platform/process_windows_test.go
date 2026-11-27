package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestMain(m *testing.M) {
	if handled, code := ProcessHelper(); handled {
		os.Exit(code)
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--test-argv":
			_ = json.NewEncoder(os.Stdout).Encode(os.Args[2:])
			os.Exit(31)
		case "--test-tree":
			child := exec.Command(os.Args[0], "--test-wait")
			if err := child.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Println(child.Process.Pid)
			_ = child.Wait()
			os.Exit(0)
		case "--test-wait":
			time.Sleep(time.Minute)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func TestNativeCLIArgumentsAndExitCode(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"空 格", "", `quotes " and \`, "a&b"}
	cmd, err := CLICommand(t.Context(), exe, append([]string{"--test-argv"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 31 {
		t.Fatalf("exit: %v; output: %s", err, out)
	}
	var received []string
	if err = json.Unmarshal(out, &received); err != nil || !reflect.DeepEqual(received, args) {
		t.Fatalf("received %q: %v", out, err)
	}
}

func TestCancelLocalJobDestroysDescendants(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := CLICommand(ctx, exe, "--test-tree")
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	scanner := bufio.NewScanner(out)
	if !scanner.Scan() {
		t.Fatalf("missing descendant PID: %v", scanner.Err())
	}
	pid, err := strconv.ParseUint(scanner.Text(), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	cancel()
	event, err := windows.WaitForSingleObject(h, 5000)
	if err != nil || event != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant survived cancellation: %d, %v", event, err)
	}
}

func TestImageContainsOnlyInterpreterAndStandardLibrary(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"python.exe", "python313.dll", "vcruntime140_1.dll", "extra.dll", "DLLs/_ssl.pyd", "DLLs/dev.key", "Lib/json/__init__.py", "Lib/json/config.ini", "Lib/user_settings.py", "Lib/site-packages/secret.py", "Lib/personal.pth", "Scripts/token.exe"} {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, _, err := imageFiles(t.Context(), filepath.Join(dir, "python.exe"), map[string]bool{"json": true})
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, file := range files {
		found = append(found, filepath.ToSlash(file.relative))
	}
	want := []string{"DLLs/_ssl.pyd", "Lib/json/__init__.py", "python.exe", "python313.dll", "vcruntime140_1.dll"}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("got %q, want %q", found, want)
	}
}
