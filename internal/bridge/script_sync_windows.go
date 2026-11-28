package bridge

import "golang.org/x/sys/windows"

// Windows publishes the file entry with MOVEFILE_WRITE_THROUGH.
func syncScriptDir(string) error { return nil }
func publishScriptFile(from, to string) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(src, dst, windows.MOVEFILE_WRITE_THROUGH)
}
