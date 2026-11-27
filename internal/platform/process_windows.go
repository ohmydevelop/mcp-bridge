// Windows starts local children through a short-lived supervisor. It is the sole
// owner of a kill-on-close job; losing the supervisor tears down its entire tree.
package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	supervisorFlag                = "--bridge-windows-process"
	attributeSecurityCapabilities = 0x20009 // PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES
	attributeJobList              = 0x2000d // PROC_THREAD_ATTRIBUTE_JOB_LIST
)

type securityCapabilities struct {
	Package      *windows.SID
	Capabilities *windows.SIDAndAttributes
	Count        uint32
	Reserved     uint32
}

func CLICommand(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
	return supervisedCommand(ctx, "cli", path, args)
}

func SandboxCommand(ctx context.Context, python string, args ...string) (*exec.Cmd, error) {
	image, err := pythonImage(ctx, python)
	if err != nil {
		return nil, fmt.Errorf("prepare isolated Python runtime: %w", err)
	}
	cmd, err := supervisedCommand(ctx, "python", image, args)
	if err == nil {
		cmd.Dir = filepath.Dir(image)
		// Windows resolves these user folders while creating the AppContainer.
		cmd.Env = []string{}
		for _, key := range []string{"SystemRoot", "SystemDrive", "WINDIR", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "TEMP", "TMP", "ProgramData", "ProgramFiles", "ProgramFiles(x86)"} {
			if value, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}
	}
	return cmd, err
}

func supervisedCommand(ctx context.Context, kind, path string, args []string) (*exec.Cmd, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("native child executable must be absolute")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	stamp, err := creationStamp(windows.CurrentProcess())
	if err != nil {
		return nil, err
	}
	parameters := []string{supervisorFlag, kind, strconv.Itoa(os.Getpid()), stamp, path}
	cmd := exec.CommandContext(ctx, exe, append(parameters, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd, nil
}

func creationStamp(process windows.Handle) (string, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &creation, &exit, &kernel, &user); err != nil {
		return "", err
	}
	stamp := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	return strconv.FormatUint(stamp, 16), nil
}

// ProcessHelper must run before flag parsing in the executable hosting Bridge.
func ProcessHelper() (bool, int) {
	if len(os.Args) < 2 || os.Args[1] != supervisorFlag {
		return false, 0
	}
	code, err := supervise(os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Windows process supervisor:", err)
		return true, 125
	}
	return true, int(code)
}

func supervise(args []string) (uint32, error) {
	if len(args) < 4 || (args[0] != "cli" && args[0] != "python") || !filepath.IsAbs(args[3]) {
		return 0, errors.New("invalid process arguments")
	}
	pid, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		return 0, err
	}
	parent, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(parent)
	stamp, err := creationStamp(parent)
	if err != nil {
		return 0, err
	}
	if stamp != args[2] {
		return 0, errors.New("originating process has exited")
	}
	isolated := args[0] == "python"
	var security securityCapabilities
	if isolated {
		sid, cleanup, err := workerProfile()
		if err != nil {
			return 0, err
		}
		defer cleanup()
		security.Package = sid
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(job)
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if isolated {
		limits.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS | windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		limits.BasicLimitInformation.ActiveProcessLimit = 1
		limits.ProcessMemoryLimit = 256 << 20
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return 0, err
	}
	attrs, err := windows.NewProcThreadAttributeList(3)
	if err != nil {
		return 0, err
	}
	defer attrs.Delete()
	if err = attrs.Update(attributeJobList, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		return 0, err
	}
	if isolated {
		if err = attrs.Update(attributeSecurityCapabilities, unsafe.Pointer(&security), unsafe.Sizeof(security)); err != nil {
			return 0, err
		}
	}
	var streams [3]windows.Handle
	for i, file := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		err = windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(file.Fd()), windows.CurrentProcess(), &streams[i], 0, true, windows.DUPLICATE_SAME_ACCESS)
		if err != nil {
			return 0, err
		}
		defer windows.CloseHandle(streams[i])
	}
	if err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&streams[0]), unsafe.Sizeof(streams)); err != nil {
		return 0, err
	}
	var info windows.StartupInfoEx
	info.Cb = uint32(unsafe.Sizeof(info))
	info.Flags = windows.STARTF_USESTDHANDLES
	info.StdInput, info.StdOutput, info.StdErr = streams[0], streams[1], streams[2]
	info.ProcThreadAttributeList = attrs.List()
	image, err := windows.UTF16PtrFromString(args[3])
	if err != nil {
		return 0, err
	}
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args[3:]))
	if err != nil {
		return 0, err
	}
	var child windows.ProcessInformation
	err = windows.CreateProcess(image, command, nil, nil, true, windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_NO_WINDOW, nil, nil, &info.StartupInfo, &child)
	runtime.KeepAlive(security)
	if err != nil {
		return 0, fmt.Errorf("create %s process: %w", args[0], err)
	}
	windows.CloseHandle(child.Thread)
	defer windows.CloseHandle(child.Process)
	event, err := windows.WaitForMultipleObjects([]windows.Handle{parent, child.Process}, false, windows.INFINITE)
	if err != nil {
		return 0, err
	}
	if event == windows.WAIT_OBJECT_0 {
		return 0, errors.New("originating process exited")
	}
	var code uint32
	err = windows.GetExitCodeProcess(child.Process, &code)
	return code, err
}
