package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"newmcp/mcp-bridge/internal/plugin"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

type sourceRequest struct {
	Source Source `json:"source"`
	Path   string `json:"package_path,omitempty"`
	Dry    bool   `json:"dry_run,omitempty"`
}

func (b *Bridge) sourceTools(v *view) {
	tool(v.server, "bridge_create_source", "Connect a new MCP source or install a local plugin. Use dry_run to inspect choices and settings.", func(ctx context.Context, in sourceRequest) (any, error) { return b.configure(ctx, v.user, in, 0) })
	tool(v.server, "bridge_update_source", "Patch a source by ID and revision. Omitted fields are retained. Supply package_path to upgrade its plugin.", func(ctx context.Context, in struct {
		ID       string `json:"source_id"`
		Revision int    `json:"revision"`
		Patch    Object `json:"patch"`
		Path     string `json:"package_path,omitempty"`
		Dry      bool   `json:"dry_run,omitempty"`
	}) (any, error) {
		var s Source
		rev, e := b.store.Get("source", in.ID, &s)
		if e != nil {
			return nil, e
		}
		if rev != in.Revision {
			return nil, errors.New("revision conflict")
		}
		if _, ok := in.Patch["id"]; ok {
			return nil, errors.New("source_id is immutable")
		}
		base, e := json.Marshal(s)
		if e != nil {
			return nil, e
		}
		var fields Object
		if e = json.Unmarshal(base, &fields); e != nil {
			return nil, e
		}
		mergePatch(fields, in.Patch)
		raw, e := json.Marshal(fields)
		if e != nil {
			return nil, e
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		s = Source{}
		if e = dec.Decode(&s); e != nil {
			return nil, e
		}
		if s.ID != in.ID {
			return nil, errors.New("source_id is immutable")
		}
		return b.configure(ctx, v.user, sourceRequest{s, in.Path, in.Dry}, rev)
	})
}

// Object members merge recursively. Null removes a member and arrays replace in full.
func mergePatch(fields Object, patch Object) {
	for key, value := range patch {
		if value == nil {
			delete(fields, key)
			continue
		}
		if child, ok := value.(Object); ok {
			existing, _ := fields[key].(Object)
			if existing == nil {
				existing = Object{}
			}
			mergePatch(existing, child)
			fields[key] = existing
		} else {
			fields[key] = value
		}
	}
}

func (b *Bridge) configure(ctx context.Context, p Principal, in sourceRequest, rev int) (any, error) {
	s := in.Source
	if !identifier.MatchString(s.ID) {
		return nil, errors.New("valid source.id required")
	}
	if in.Path != "" {
		base := filepath.Join(b.dir, "packages")
		if e := os.MkdirAll(base, 0700); e != nil {
			return nil, e
		}
		stage, e := os.MkdirTemp(base, "stage-")
		if e != nil {
			return nil, e
		}
		defer os.RemoveAll(stage)
		id, e := plugin.Snapshot(in.Path, stage)
		if e != nil {
			return nil, e
		}
		pkg, e := plugin.InspectDir(stage)
		if e != nil {
			return nil, e
		}
		var servers []string
		for n := range pkg.Servers {
			servers = append(servers, n)
		}
		sort.Strings(servers)
		if s.Type == "" || s.Type == "package" {
			s.Type = "package"
			if s.Server == "" && len(servers) == 1 {
				s.Server = servers[0]
			}
		}
		if s.Server != "" && !slices.Contains(servers, s.Server) {
			return nil, errors.New("unknown packaged server")
		}
		missing := []string{}
		for k, def := range pkg.Settings {
			if def.Required && def.Default == nil && s.Settings[k] == nil {
				missing = append(missing, k)
			}
		}
		sort.Strings(missing)
		for k, val := range s.Settings {
			def, ok := pkg.Settings[k]
			if !ok {
				return nil, fmt.Errorf("unknown setting %s", k)
			}
			if _, err := configValue(val, def); err != nil {
				return nil, fmt.Errorf("setting %s: %w", k, err)
			}
		}
		if in.Dry || len(missing) > 0 || (s.Type == "package" && s.Server == "" && len(servers) > 1) {
			return Object{"state": "preview", "package": id, "servers": servers, "schema": pkg.Settings, "missing": missing}, nil
		}
		s.Package = id
		dest, _ := b.packageDir(id)
		if err := os.Rename(stage, dest); err != nil {
			if _, e = os.Stat(dest); e != nil {
				return nil, err
			}
		}
	}
	if in.Dry {
		_, e := b.resolve(s)
		return Object{"state": "preview", "source_id": s.ID}, e
	}
	return b.saveSource(ctx, p, s, rev)
}
func (b *Bridge) sourceStatus(id string) (Object, error) {
	var s Source
	rev, e := b.store.Get("source", id, &s)
	if e != nil {
		return nil, e
	}
	b.mu.RLock()
	live := b.live[id] != nil
	b.mu.RUnlock()
	out := Object{"source_id": id, "revision": rev, "type": s.Type, "ready": live || !s.Disabled && s.Type == "package" && s.Server == "", "package": s.Package, "server": s.Server, "disabled": s.Disabled, "grants": s.Grants, "hidden": s.Hidden, "events": s.Events, "aliases": s.Aliases}
	if s.Package != "" {
		dir, e := b.packageDir(s.Package)
		if e != nil {
			return nil, e
		}
		pkg, e := plugin.InspectDir(dir)
		if e != nil {
			return nil, e
		}
		values := Object{}
		for k, v := range s.Settings {
			if def, ok := pkg.Settings[k]; ok && !def.Sensitive {
				values[k] = v
			}
		}
		out["schema"] = pkg.Settings
		out["values"] = values
	}
	return out, nil
}

// Called with the source catalog read-locked.
func (b *Bridge) sourceCapabilities(src Source) []string {
	caps := []string{}
	if src.Package != "" {
		caps = append(caps, "plugin")
	}
	if len(src.Events) > 0 {
		caps = append(caps, "adapter_events")
	}
	if r := b.live[src.ID]; r != nil {
		if len(r.tools) > 0 {
			caps = append(caps, "tools")
		}
		if len(r.resources) > 0 {
			caps = append(caps, "resources")
		}
		if len(r.prompts) > 0 {
			caps = append(caps, "prompts")
		}
		for ext := range r.session.InitializeResult().Capabilities.Extensions {
			caps = append(caps, ext)
		}
	}
	sort.Strings(caps)
	return caps
}
