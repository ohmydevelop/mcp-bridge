package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

var userenv = windows.NewLazySystemDLL("userenv.dll")
var createProfile = userenv.NewProc("CreateAppContainerProfile")
var deleteProfile = userenv.NewProc("DeleteAppContainerProfile")

// Each worker gets a fresh private profile, including its temporary directory.
func workerProfile() (*windows.SID, func(), error) {
	name, err := windows.UTF16PtrFromString("MCPBridge." + uuid.NewString())
	if err != nil {
		return nil, nil, err
	}
	n := uintptr(unsafe.Pointer(name))
	var sid *windows.SID
	hr, _, _ := createProfile.Call(n, n, n, 0, 0, uintptr(unsafe.Pointer(&sid)))
	runtime.KeepAlive(name)
	if int32(hr) < 0 {
		return nil, nil, fmt.Errorf("create AppContainer profile: HRESULT %#08x", uint32(hr))
	}
	return sid, func() {
		windows.FreeSid(sid)
		deleteProfile.Call(uintptr(unsafe.Pointer(name)))
		runtime.KeepAlive(name)
	}, nil
}

var pythonDLL = regexp.MustCompile(`^(python3[0-9]*t?|vcruntime[0-9_]+)\.dll$`)

func standardModules(ctx context.Context, python string) (map[string]bool, error) {
	probe := exec.CommandContext(ctx, python, "-ISB", "-c", "import json,sys;print(json.dumps(sorted(sys.stdlib_module_names)))")
	probe.Dir = filepath.Dir(python)
	probe.Env = []string{}
	data, err := probe.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect CPython standard library (Python 3.10+): %w", err)
	}
	var names []string
	if err = json.Unmarshal(data, &names); err != nil {
		return nil, err
	}
	modules := make(map[string]bool, len(names))
	for _, n := range names {
		modules[strings.ToLower(n)] = true
	}
	return modules, nil
}

type imageFile struct {
	relative string
	info     fs.FileInfo
}

// Only the standard CPython layout is included. The interpreter view inherits
// no permissions from the original Python installation or from user packages.
func imageFiles(ctx context.Context, python string, modules map[string]bool) ([]imageFile, string, error) {
	dir := filepath.Dir(python)
	base := filepath.Base(python)
	var files []imageFile
	hash := sha256.New()
	fmt.Fprintf(hash, "v3\x00%s\x00", python)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		parts := strings.Split(strings.ToLower(rel), string(filepath.Separator))
		name := strings.ToLower(entry.Name())
		if entry.IsDir() {
			if parts[0] != "lib" && parts[0] != "dlls" {
				return filepath.SkipDir
			}
			if name == "__pycache__" || name == "test" || name == "tests" || strings.EqualFold(name, "site-packages") {
				return filepath.SkipDir
			}
			if parts[0] == "lib" && len(parts) > 1 && !modules[parts[1]] {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		switch {
		case len(parts) == 1:
			if !strings.EqualFold(name, base) && !pythonDLL.MatchString(name) {
				return nil
			}
		case parts[0] == "dlls":
			if len(parts) != 2 || (ext != ".dll" && ext != ".pyd") {
				return nil
			}
		case parts[0] == "lib":
			if ext != ".py" || !modules[strings.TrimSuffix(parts[1], ".py")] {
				return nil
			}
		default:
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if raw, ok := info.Sys().(*syscall.Win32FileAttributeData); ok && raw.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("unsupported reparse point in Python runtime: %s", rel)
		}
		files = append(files, imageFile{rel, info})
		fmt.Fprintf(hash, "%s\x00%d\x00%d\n", rel, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return files, hex.EncodeToString(hash.Sum(nil)), err
}

func readOnlyView(dir string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid, err := windows.CreateWellKnownSid(windows.WinBuiltinAnyPackageSid)
	if err != nil {
		return err
	}
	sddl := fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FRFX;;;%s)", user.User.Sid.String(), sid.String())
	desc, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	acl, _, err := desc.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// Standard per-user CPython installs are not readable by an AppContainer.
// Publish a snapshot atomically; no ACLs on the user's installation are changed.
func pythonImage(ctx context.Context, python string) (string, error) {
	if !filepath.IsAbs(python) {
		return "", errors.New("expected absolute Python executable")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(python), "Lib", "encodings", "__init__.py")); err != nil {
		return "", fmt.Errorf("unsupported CPython layout: %w", err)
	}
	modules, err := standardModules(ctx, python)
	if err != nil {
		return "", err
	}
	files, sum, err := imageFiles(ctx, python, modules)
	if err != nil {
		return "", err
	}
	appdata, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return "", err
	}
	cache := filepath.Join(appdata, "MCPBridge", "PythonRuntimes")
	if err = os.MkdirAll(cache, 0700); err != nil {
		return "", err
	}
	dest := filepath.Join(cache, sum)
	image := filepath.Join(dest, filepath.Base(python))
	if _, err = os.Stat(image); err == nil {
		return image, nil
	}
	stage, err := os.MkdirTemp(cache, "staging-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err = readOnlyView(stage); err != nil {
		return "", err
	}
	src, err := os.OpenRoot(filepath.Dir(python))
	if err != nil {
		return "", err
	}
	defer src.Close()
	for _, file := range files {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		target := filepath.Join(stage, file.relative)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return "", err
		}
		if err = copyRuntimeFile(src, file, target); err != nil {
			return "", err
		}
	}
	if err = os.WriteFile(filepath.Join(stage, strings.TrimSuffix(filepath.Base(python), filepath.Ext(python))+"._pth"), []byte("Lib\r\nDLLs\r\n.\r\n"), 0600); err != nil {
		return "", err
	}
	if _, err = os.Stat(filepath.Join(stage, filepath.Base(python))); err != nil {
		return "", err
	}
	if err = os.Rename(stage, dest); err != nil {
		// Another instance may have published the same immutable view.
		if _, existing := os.Stat(image); existing != nil {
			return "", err
		}
	}
	return image, nil
}

func copyRuntimeFile(source *os.Root, file imageFile, dest string) error {
	in, err := source.Open(file.relative)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if n != file.info.Size() {
		return fmt.Errorf("Python file changed during setup: %s", file.relative)
	}
	return closeErr
}
