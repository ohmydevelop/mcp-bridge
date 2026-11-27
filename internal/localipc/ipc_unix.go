//go:build linux || darwin

package localipc

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func privateDir(dir string) error {
	unix.Umask(0077)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("state directory must be private (0700): %s", dir)
	}
	return nil
}

func lockDir(dir string) (io.Closer, error) {
	f, err := os.OpenFile(filepath.Join(dir, "bridge.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock state directory: %w", err)
	}
	return f, nil
}

// Long macOS temporary directory names can exceed sockaddr_un.sun_path.
func socketPath(dir string) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(real, "bridge.sock")
	var addr unix.RawSockaddrUnix
	if len(path) < len(addr.Path) {
		return path, nil
	}
	base := fmt.Sprintf("/tmp/mcp-bridge-%d", os.Getuid())
	if err := os.Mkdir(base, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	var st unix.Stat_t
	if err := unix.Lstat(base, &st); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || int(st.Uid) != os.Getuid() || st.Mode&0077 != 0 {
		return "", fmt.Errorf("unsafe local socket directory %s", base)
	}
	sum := sha256.Sum256([]byte(real))
	return filepath.Join(base, fmt.Sprintf("%x.sock", sum[:20])), nil
}

func Listen(dir string) (net.Listener, error) {
	path, err := socketPath(dir)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return net.Listen("unix", path)
}

func Dial(dir string) (Conn, error) {
	path, err := socketPath(dir)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, err
	}
	return c.(*net.UnixConn), nil
}
