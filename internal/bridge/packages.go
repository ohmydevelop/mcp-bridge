package bridge

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"strings"

	"newmcp/mcp-bridge/internal/plugin"
)

var packageID = regexp.MustCompile("^[a-f0-9]{64}$")
var tokenVar = regexp.MustCompile(`\$\{([^}]+)\}`)

func (b *Bridge) packageDir(id string) (string, error) {
	if !packageID.MatchString(id) {
		return "", errors.New("invalid package digest")
	}
	return filepath.Join(b.dir, "packages", id), nil
}
func (b *Bridge) resolve(s Source) (Source, error) {
	if s.Package != "" {
		dir, e := b.packageDir(s.Package)
		if e != nil {
			return s, e
		}
		p, e := plugin.InspectDir(dir)
		if e != nil {
			return s, e
		}
		vars := map[string]string{"PLUGIN_ROOT": dir, "__dirname": dir, "pathSeparator": string(os.PathSeparator), "PLUGIN_DATA": filepath.Join(b.dir, "instances", s.ID)}
		if e = os.MkdirAll(vars["PLUGIN_DATA"], 0700); e != nil {
			return s, e
		}
		vars["/"] = string(os.PathSeparator)
		if home, err := os.UserHomeDir(); err == nil {
			vars["HOME"] = home
			for key, folder := range map[string]string{"DESKTOP": "Desktop", "DOCUMENTS": "Documents", "DOWNLOADS": "Downloads"} {
				vars[key] = filepath.Join(home, folder)
			}
		}
		multi := map[string][]string{}
		for k := range s.Settings {
			if _, ok := p.Settings[k]; !ok {
				return s, fmt.Errorf("unknown setting %s", k)
			}
		}
		for k, def := range p.Settings {
			val, ok := s.Settings[k]
			if !ok {
				val = def.Default
			}
			if def.Required && val == nil {
				return s, fmt.Errorf("missing setting %s", k)
			}
			if val != nil {
				vars["user_config."+k], e = configValue(val, def)
				if def.Multiple {
					if vals, ok := val.([]any); ok {
						var parts []string
						for _, v := range vals {
							if str, ok := v.(string); ok {
								parts = append(parts, str)
							}
						}
						multi["${user_config."+k+"}"] = parts
					}
				}
				if e != nil {
					return s, fmt.Errorf("setting %s: %w", k, e)
				}
			}
		}
		if s.Server != "" {
			cfg, ok := p.Servers[s.Server]
			if !ok {
				return s, errors.New("unknown packaged server")
			}
			s.Type, s.Command, s.Args, s.Cwd, s.URL, s.Headers, s.Env = cfg.Type, cfg.Command, cfg.Args, cfg.Cwd, cfg.URL, cfg.Headers, cfg.Env
			expand := func(value string) string {
				return tokenVar.ReplaceAllStringFunc(value, func(expr string) string {
					key := expr[2 : len(expr)-1]
					v, ok := vars[key]
					if !ok {
						e = fmt.Errorf("unresolved package variable %s", key)
					}
					return v
				})
			}
			s.URL = expand(s.URL)
			s.Command = expand(s.Command)
			s.Cwd = expand(s.Cwd)
			if s.Cwd == "" {
				s.Cwd = dir
			} else if !filepath.IsAbs(s.Cwd) {
				s.Cwd = filepath.Join(dir, s.Cwd)
			}
			if strings.HasPrefix(s.Command, "./") {
				s.Command = filepath.Join(dir, s.Command)
			}
			if cfg.Cwd != "" {
				root := dir
				if strings.HasPrefix(cfg.Cwd, "${PLUGIN_DATA}") {
					root = vars["PLUGIN_DATA"]
				}
				actual, err := filepath.EvalSymlinks(s.Cwd)
				if err != nil {
					return s, err
				}
				rel, err := filepath.Rel(root, actual)
				if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
					return s, errors.New("package working directory escapes its root")
				}
			}
			args := []string{}
			for _, v := range s.Args {
				if parts, ok := multi[v]; ok {
					args = append(args, parts...)
				} else {
					args = append(args, expand(v))
				}
			}
			s.Args = args
			for k, v := range s.Env {
				s.Env[k] = expand(v)
			}
			for k, v := range s.Headers {
				s.Headers[k] = expand(v)
			}
			if e != nil {
				return s, e
			}
		}
	}
	switch s.Type {
	case "stdio":
		if s.Command == "" {
			return s, errors.New("command required")
		}
	case "streamable-http", "sse":
		u, e := url.Parse(s.URL)
		if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return s, errors.New("invalid MCP endpoint")
		}
	case "package":
		if s.Package == "" {
			return s, errors.New("package digest required")
		}
	default:
		return s, fmt.Errorf("unsupported source type %q on %s", s.Type, runtime.GOOS)
	}
	return s, nil
}
