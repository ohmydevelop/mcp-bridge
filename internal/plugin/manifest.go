package plugin

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/dlclark/regexp2"
	js "github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/*.json
var schemas embed.FS

type Server struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
}

type Setting struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Sensitive   bool   `json:"sensitive"`
	Default     any    `json:"default,omitempty"`
	Multiple    bool   `json:"multiple,omitempty"`
}
type Package struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	Version  string             `json:"version"`
	Settings map[string]Setting `json:"user_config,omitempty"`
	Servers  map[string]Server  `json:"-"`
}

func validateDoc(name string, raw []byte) error {
	schema, e := schemas.ReadFile("schemas/" + name + ".json")
	if e != nil {
		return e
	}
	d, e := js.UnmarshalJSON(bytes.NewReader(schema))
	if e != nil {
		return e
	}
	c := js.NewCompiler()
	c.UseRegexpEngine(func(pattern string) (js.Regexp, error) {
		r, e := regexp2.Compile(pattern, regexp2.ECMAScript)
		return ecmaPattern{r}, e
	})
	const id = "urn:bridge:validation"
	if e = c.AddResource(id, d); e != nil {
		return e
	}
	s, e := c.Compile(id)
	if e != nil {
		return e
	}
	v, e := js.UnmarshalJSON(bytes.NewReader(raw))
	if e != nil {
		return e
	}
	return s.Validate(v)
}
func InspectDir(dir string) (Package, error) {
	var p Package
	raw, e := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if e == nil {
		if e = validateDoc("plugin", raw); e != nil {
			return p, e
		}
		if e = json.Unmarshal(raw, &p); e != nil {
			return p, e
		}
		raw, e = os.ReadFile(filepath.Join(dir, "mcp.json"))
		if errors.Is(e, fs.ErrNotExist) {
			return p, nil
		}
		if e != nil {
			return p, e
		}
		if e = validateDoc("mcp", raw); e != nil {
			return p, e
		}
		var cfg struct {
			Servers map[string]Server `json:"mcpServers"`
		}
		e = json.Unmarshal(raw, &cfg)
		p.Servers = cfg.Servers
		return p, e
	}
	if !errors.Is(e, fs.ErrNotExist) {
		return p, e
	}
	raw, e = os.ReadFile(filepath.Join(dir, "manifest.json"))
	if e != nil {
		return p, e
	}
	var m struct {
		Name, Version string
		Format        string             `json:"manifest_version"`
		Legacy        string             `json:"dxt_version"`
		Settings      map[string]Setting `json:"user_config"`
		Server        struct {
			Config struct {
				Server
				Overrides map[string]Server `json:"platform_overrides"`
			} `json:"mcp_config"`
		}
	}
	if e = json.Unmarshal(raw, &m); e != nil {
		return p, e
	}
	if m.Format == "" {
		m.Format = m.Legacy
	}
	if !slices.Contains([]string{"0.1", "0.2", "0.3", "0.4"}, m.Format) {
		return p, errors.New("unsupported MCPB manifest version")
	}
	if e = validateDoc("mcpb-manifest-v"+m.Format+".schema", raw); e != nil {
		return p, e
	}
	cfg := m.Server.Config.Server
	if over, ok := m.Server.Config.Overrides[runtime.GOOS]; ok {
		if over.Command != "" {
			cfg.Command = over.Command
		}
		if over.Args != nil {
			cfg.Args = over.Args
		}
		if cfg.Env == nil {
			cfg.Env = map[string]string{}
		}
		for k, v := range over.Env {
			cfg.Env[k] = v
		}
	}
	cfg.Type = "stdio"
	return Package{Name: m.Name, Version: m.Version, Servers: map[string]Server{"default": cfg}, Settings: m.Settings}, nil
}

type ecmaPattern struct{ *regexp2.Regexp }

func (r ecmaPattern) MatchString(s string) bool {
	m, e := r.Regexp.MatchString(s)
	return e == nil && m
}
