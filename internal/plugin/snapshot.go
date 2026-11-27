package plugin

import (
	"archive/zip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Snapshot only ordinary files. The digest includes executable mode and file boundaries.
func Snapshot(path, dir string) (string, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return "", e
	}
	var files fs.FS
	var archive *zip.ReadCloser
	if st.IsDir() {
		files = os.DirFS(path)
	} else if st.Mode().IsRegular() {
		archive, e = zip.OpenReader(path)
		if e != nil {
			return "", e
		}
		defer archive.Close()
		seen := map[string]bool{}
		for _, f := range archive.File {
			n := strings.TrimSuffix(f.Name, "/")
			if !fs.ValidPath(n) || n == "." || strings.ContainsAny(n, "\\\x00") || seen[n] || len(seen) >= 10000 || (!f.Mode().IsRegular() && !f.Mode().IsDir()) {
				return "", errors.New("invalid or duplicate archive entry")
			}
			seen[n] = true
		}
		files = archive
	} else {
		return "", errors.New("expected package directory or zip")
	}
	h := sha256.New()
	var total int64
	count := 0
	e = fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if !fs.ValidPath(name) || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
			return errors.New("invalid archive path")
		}
		count++
		if count > 10000 {
			return errors.New("too many package entries")
		}
		dest := filepath.Join(dir, name)
		if d.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("packages may contain only regular files and directories")
		}
		if info.Size() > 64<<20 || total+info.Size() > 128<<20 {
			return errors.New("package too large")
		}
		f, e := files.Open(name)
		if e != nil {
			return e
		}
		defer f.Close()
		mode := fs.FileMode(0600)
		if info.Mode()&0111 != 0 {
			mode = 0700
		}
		out, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if e != nil {
			return e
		}
		fmt.Fprintf(h, "%d:%s/%d/%d:", len(name), name, mode, info.Size())
		n, e := io.Copy(io.MultiWriter(h, out), io.LimitReader(f, info.Size()+1))
		ce := out.Close()
		total += n
		if e != nil {
			return e
		}
		if n != info.Size() {
			return errors.New("package size changed")
		}
		return ce
	})
	return fmt.Sprintf("%x", h.Sum(nil)), e
}
