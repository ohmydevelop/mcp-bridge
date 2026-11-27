//go:build darwin || windows

package pyrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Resolve through the candidate interpreter: version managers and Windows py are launchers.
const identify = `import json, os, sys, sysconfig
if sys.version_info.major != 3 or sys.implementation.name != "cpython" or (sys.platform=="win32" and sys.version_info < (3, 10)):
    raise SystemExit(2)
path = os.path.realpath(getattr(sys, "_base_executable", None) or sys.executable)
def lib(directory, name):
    if not directory or not name:
        return ""
    return os.path.realpath(os.path.join(directory, name))
print(json.dumps(dict(executable=path, stdlib=sysconfig.get_path("stdlib"), platstdlib=sysconfig.get_path("platstdlib",vars={"platbase":sys.base_exec_prefix}),
                      library=lib(sysconfig.get_config_var("LIBDIR"), sysconfig.get_config_var("LDLIBRARY")),
                      framework=lib(sys.base_prefix,sysconfig.get_config_var("PYTHONFRAMEWORK")))))`

func discover(ctx context.Context) (interpreter, error) {
	return resolve(ctx, append(pathCandidates(), systemCandidates()...))
}

func pathCandidates() [][]string {
	var candidates [][]string
	if hint := installedHint(); hint != "" {
		candidates = append(candidates, []string{hint})
	}
	if hint := os.Getenv("BRIDGE_PYTHON"); filepath.IsAbs(hint) {
		candidates = append(candidates, []string{hint})
	}
	for _, name := range []string{"python", "python3"} {
		if path, err := exec.LookPath(name); err == nil {
			candidates = append(candidates, []string{path})
		}
	}
	if runtime.GOOS == "windows" {
		if path, err := exec.LookPath("py"); err == nil {
			candidates = append(candidates, []string{path, "-3"})
		}
	}
	return candidates
}

// The installer can atomically publish an updated shell discovery result while
// the daemon is running. Contents name an executable, never a shell command.
func installedHint() string {
	path := os.Getenv("BRIDGE_PYTHON_HINT_FILE")
	if !filepath.IsAbs(path) {
		return ""
	}
	const maxHint = 128 << 10
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Size() > maxHint {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxHint+1))
	if err != nil || len(raw) > maxHint {
		return ""
	}
	hint := strings.TrimRight(strings.TrimPrefix(string(raw), "\uFEFF"), "\r\n")
	if !filepath.IsAbs(hint) || strings.ContainsAny(hint, "\x00\r\n") {
		return ""
	}
	return hint
}

func resolve(ctx context.Context, candidates [][]string) (interpreter, error) {
	var last error
	for _, candidate := range candidates {
		path := candidate[0]
		if !filepath.IsAbs(path) {
			continue
		}
		if ctx.Err() != nil {
			return interpreter{}, ctx.Err()
		}
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		cmd := exec.CommandContext(probe, path, append(candidate[1:], "-ISB", "-c", identify)...)
		cmd.Env = slices.DeleteFunc(os.Environ(), func(variable string) bool {
			key, _, _ := strings.Cut(variable, "=")
			return strings.HasPrefix(strings.ToUpper(key), "BRIDGE_")
		})
		cmd.WaitDelay = 250 * time.Millisecond
		info, err := cmd.Output()
		cancel()
		var py interpreter
		if err == nil {
			err = json.Unmarshal(info, &py)
		}
		if err == nil {
			if !filepath.IsAbs(py.Executable) || !filepath.IsAbs(py.Stdlib) || !filepath.IsAbs(py.Platstdlib) {
				err = errors.New("interpreter reported invalid paths")
			} else if _, statErr := exec.LookPath(py.Executable); statErr != nil {
				err = statErr
			} else {
				return py, nil
			}
		}
		last = fmt.Errorf("%s: %w", strings.Join(candidate, " "), err)
	}
	if ctx.Err() != nil {
		return interpreter{}, ctx.Err()
	}
	if last != nil {
		return interpreter{}, fmt.Errorf("cannot resolve native Python 3: %w", last)
	}
	return interpreter{}, errors.New("Python 3 was not found on PATH; install Python or set BRIDGE_PYTHON")
}
