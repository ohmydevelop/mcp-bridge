package platform

import (
	"os"
	"runtime"
)

// ChildEnv excludes Bridge credentials from upstream processes and CLI deliveries.
func ChildEnv(includeHome bool) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	if includeHome {
		if home, err := os.UserHomeDir(); err == nil {
			env = append(env, "HOME="+home)
		}
	}
	if runtime.GOOS == "windows" {
		env = append(env, "PYTHONIOENCODING=utf-8")
		for _, key := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "PATHEXT"} {
			if value, ok := os.LookupEnv(key); ok {
				env = append(env, key+"="+value)
			}
		}
	}
	return env
}
