package localipc

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func privateDir(dir string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	sid := user.User.Sid
	sd, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(name, &sa); err == nil {
		return nil
	} else if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	existing, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := existing.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		return fmt.Errorf("state directory must be owned by the current user: %s", dir)
	}
	acl, _, err := existing.DACL()
	if err != nil || acl == nil {
		return fmt.Errorf("private state directory ACL required: %s", dir)
	}
	for i := uint16(0); i < acl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(i), &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(trustee.Equals(sid) || trustee.IsWellKnown(windows.WinLocalSystemSid) || trustee.IsWellKnown(windows.WinBuiltinAdministratorsSid)) {
			return fmt.Errorf("state directory grants access outside the current user, SYSTEM, or Administrators: %s", dir)
		}
	}
	return nil
}

func lockDir(dir string) (io.Closer, error) {
	f, err := os.OpenFile(filepath.Join(dir, "bridge.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock state directory: %w", err)
	}
	return f, nil
}

func endpoint(dir string) (string, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", nil, err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", nil, err
	}
	sid := user.User.Sid
	digest := sha256.Sum256([]byte(sid.String() + "\x00" + strings.ToLower(filepath.Clean(resolved))))
	return fmt.Sprintf(`\\.\pipe\mcp-bridge-%x`, digest[:20]), sid, nil
}

func Listen(dir string) (net.Listener, error) {
	name, sid, err := endpoint(dir)
	if err != nil {
		return nil, err
	}
	owner := sid.String()
	return winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: "O:" + owner + "D:P(A;;GA;;;" + owner + ")", MessageMode: true})
}

func Dial(dir string) (Conn, error) {
	name, sid, err := endpoint(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := winio.DialPipeAccess(ctx, name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL)
	if err != nil {
		return nil, err
	}
	// Verify ownership on the connected handle before sending any MCP traffic.
	pipe, ok := c.(interface {
		Conn
		Fd() uintptr
	})
	if !ok {
		c.Close()
		return nil, errors.New("incompatible Bridge pipe")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(pipe.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		c.Close()
		return nil, err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		c.Close()
		return nil, errors.New("Bridge pipe owner does not match current user")
	}
	return pipe, nil
}
