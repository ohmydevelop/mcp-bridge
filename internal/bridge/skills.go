package bridge

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

type SkillFile struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
	Size   int    `json:"size"`
}

// A nil manifest represents the protocol's explicit dynamic variant.
type skillFiles []SkillFile

func (f *skillFiles) UnmarshalJSON(raw []byte) error {
	if string(raw) == "\"dynamic\"" {
		*f = nil
		return nil
	}
	var items []SkillFile
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	if items == nil {
		return errors.New("missing skill manifest")
	}
	*f = items
	return nil
}
func (f skillFiles) MarshalJSON() ([]byte, error) {
	if f == nil {
		return []byte("\"dynamic\""), nil
	}
	return json.Marshal([]SkillFile(f))
}
func skillPath(path string) bool {
	decoded, err := url.PathUnescape(path)
	return err == nil && path != "." && fs.ValidPath(path) && decoded != "." && fs.ValidPath(decoded) && !strings.ContainsAny(decoded, "\\?#\x00") && !strings.Contains(decoded, "%") && strings.Count(path, "/") == strings.Count(decoded, "/")
}

type Skill struct {
	URI        string     `json:"uri"`
	Front      Object     `json:"frontmatter"`
	Files      skillFiles `json:"resources"`
	bytes      map[string][]byte
	originRoot string
	original   map[string]string
	remote     *mcp.ClientSession
}

const guide = "---\nname: setup-webhook\ndescription: Route an event to an agent automation.\n---\nUse bridge_list_sources, then search_event_types. Select a configured destination and call save_trigger. Check get_trigger for delivery status."

//go:embed improve-trigger.md
var improveTrigger []byte

func localSkill(id, name string, files map[string][]byte) (Skill, error) {
	prefix := "skill://" + id + "/" + name + "/"
	s := Skill{URI: prefix + "SKILL.md", bytes: map[string][]byte{}}
	frontmatter := bytes.ReplaceAll(files["SKILL.md"], []byte("\r\n"), []byte("\n"))
	parts := bytes.SplitN(frontmatter, []byte("---\n"), 3)
	if len(parts) != 3 || len(parts[0]) != 0 {
		return s, errors.New("invalid skill frontmatter")
	}
	if e := yaml.Unmarshal(parts[1], &s.Front); e != nil {
		return s, e
	}
	if s.Front["name"] != name || s.Front["description"] == nil {
		return s, errors.New("invalid skill name or description")
	}
	for path, data := range files {
		u := prefix + path
		s.bytes[u] = data
		s.Files = append(s.Files, SkillFile{u, "sha256:" + digest(data), len(data)})
	}
	slices.SortFunc(s.Files, func(a, b SkillFile) int { return strings.Compare(a.URI, b.URI) })
	return s, nil
}
func (b *Bridge) skillCatalog(ctx context.Context, p Principal, source string) ([]Skill, error) {
	list := []Skill{}
	if source == "" || source == "bridge.builtin" {
		s, e := localSkill("bridge.builtin", "setup-webhook", map[string][]byte{"SKILL.md": []byte(guide)})
		if e != nil {
			return nil, e
		}
		improve, err := localSkill("bridge.builtin", "improve-trigger", map[string][]byte{"SKILL.md": improveTrigger})
		if err != nil {
			return nil, err
		}
		list = append(list, s, improve)
	}
	b.mu.RLock()
	sources := []Source{}
	sessions := map[string]*mcp.ClientSession{}
	for id, s := range b.sources {
		if !s.Disabled && allowed(p, s) && (source == "" || source == id) {
			sources = append(sources, s)
			if r := b.live[id]; r != nil {
				sessions[id] = r.session
			}
		}
	}
	b.mu.RUnlock()
	for _, s := range sources {
		if s.Package != "" {
			dir, e := b.packageDir(s.Package)
			if e != nil {
				return nil, e
			}
			root := filepath.Join(dir, "skills")
			entries, e := os.ReadDir(root)
			if e != nil && !errors.Is(e, fs.ErrNotExist) {
				return nil, e
			}
			for _, d := range entries {
				if !d.IsDir() || member(s.Hidden, "skill:"+d.Name()) {
					continue
				}
				files := map[string][]byte{}
				folder := os.DirFS(filepath.Join(root, d.Name()))
				e = fs.WalkDir(folder, ".", func(path string, d fs.DirEntry, e error) error {
					if e != nil {
						return e
					}
					if !d.IsDir() {
						data, err := fs.ReadFile(folder, path)
						if err != nil {
							return err
						}
						files[path] = data
					}
					return nil
				})
				if e != nil {
					return nil, e
				}
				skill, e := localSkill(s.ID, d.Name(), files)
				if e != nil {
					return nil, e
				}
				list = append(list, skill)
			}
		}
		r := sessions[s.ID]
		if r == nil {
			continue
		}
		if _, ok := r.InitializeResult().Capabilities.Extensions[skillsExt]; !ok {
			continue
		}
		cursor := ""
		seen := map[string]bool{}
		for {
			out, e := external(ctx, r, skillsExt, "skills/list", &extParams{Cursor: cursor})
			if e != nil {
				return nil, e
			}
			raw, e := json.Marshal(out["skills"])
			if e != nil {
				return nil, e
			}
			var skills []json.RawMessage
			if e = json.Unmarshal(raw, &skills); e != nil {
				return nil, e
			}
			for _, entry := range skills {
				sk, err := remoteSkill(s, r, entry)
				if err != nil {
					return nil, err
				}
				if sk.URI == "" {
					continue
				}

				list = append(list, sk)
			}
			cursor, _ = out["nextCursor"].(string)
			if cursor == "" {
				break
			}
			if seen[cursor] {
				return nil, errors.New("invalid cursor loop")
			}
			seen[cursor] = true
		}
	}
	slices.SortFunc(list, func(a, b Skill) int { return strings.Compare(a.URI, b.URI) })
	return list, nil
}
func remoteSkill(s Source, r *mcp.ClientSession, entry json.RawMessage) (Skill, error) {
	var sk Skill
	var fields map[string]json.RawMessage
	if json.Unmarshal(entry, &fields) != nil || fields["resources"] == nil {
		return Skill{}, nil
	}
	if e := json.Unmarshal(entry, &sk); e != nil {
		return Skill{}, nil
	}
	if member(s.Hidden, "skill:"+fmt.Sprint(sk.Front["name"])) {
		return Skill{}, nil
	}
	original := sk.URI
	prefix := strings.TrimSuffix(original, "SKILL.md")
	if prefix == original {
		return Skill{}, nil
	}
	name, _ := sk.Front["name"].(string)
	if !strings.HasSuffix(original, "/"+name+"/SKILL.md") || !skillPath(name) || strings.Contains(name, "/") {
		return Skill{}, invalid("invalid upstream skill URI")
	}
	local := "skill://" + s.ID + "/upstream/" + base64.RawURLEncoding.EncodeToString([]byte(original)) + "/" + url.PathEscape(name) + "/"
	sk.URI = local + "SKILL.md"
	sk.remote = r
	sk.originRoot = prefix
	sk.original = map[string]string{}
	for i, f := range sk.Files {
		if !strings.HasPrefix(f.URI, prefix) {
			return Skill{}, errors.New("skill manifest contains foreign URI")
		}
		path := strings.TrimPrefix(f.URI, prefix)
		if !skillPath(path) {
			return Skill{}, errors.New("invalid skill resource path")
		}
		if _, exists := sk.original[local+path]; exists {
			return Skill{}, errors.New("duplicate skill resource")
		}
		sk.Files[i].URI = local + path
		sk.original[local+path] = f.URI
	}
	if _, ok := sk.original[sk.URI]; sk.Files != nil && !ok {
		return Skill{}, errors.New("missing SKILL.md resource")
	}
	return sk, nil
}
func (sk Skill) read(ctx context.Context, uri string) ([]byte, error) {
	if sk.remote != nil && sk.Files == nil {
		path, ok := strings.CutPrefix(uri, strings.TrimSuffix(sk.URI, "SKILL.md"))
		if !ok || !skillPath(path) {
			return nil, invalid("invalid dynamic skill resource")
		}
		up := sk.originRoot + path
		r, err := sk.remote.ReadResource(ctx, &mcp.ReadResourceParams{URI: up})
		if err != nil {
			return nil, err
		}
		if len(r.Contents) != 1 || r.Contents[0].URI != up {
			return nil, invalid("unexpected dynamic resource response")
		}
		data := r.Contents[0].Blob
		if data == nil {
			data = []byte(r.Contents[0].Text)
		}
		if uri == sk.URI {
			s, err := localSkill("remote", fmt.Sprint(sk.Front["name"]), map[string][]byte{"SKILL.md": data})
			if err != nil || !equal(s.Front, sk.Front) {
				return nil, invalid("dynamic skill frontmatter changed")
			}
		}
		return data, nil
	}
	for _, f := range sk.Files {
		if f.URI != uri {
			continue
		}
		data := sk.bytes[uri]
		if sk.remote != nil {
			r, e := sk.remote.ReadResource(ctx, &mcp.ReadResourceParams{URI: sk.original[uri]})
			if e != nil {
				return nil, e
			}
			if len(r.Contents) != 1 || r.Contents[0].URI != sk.original[uri] {
				return nil, errors.New("unexpected resource response")
			}
			data = []byte(r.Contents[0].Text)
			if r.Contents[0].Blob != nil {
				data = r.Contents[0].Blob
			}
		}
		if len(data) != f.Size || "sha256:"+digest(data) != f.Digest {
			return nil, errors.New("skill integrity check failed")
		}
		if uri == sk.URI && sk.remote != nil {
			manifest, err := localSkill("remote", fmt.Sprint(sk.Front["name"]), map[string][]byte{"SKILL.md": data})
			if err != nil || !equal(manifest.Front, sk.Front) {
				return nil, errors.New("skill frontmatter differs from manifest")
			}
		}
		return data, nil
	}
	return nil, invalid("unlisted skill file")
}

// Reversible addresses allow clients to open a known upstream skill even when
// its server publishes a partial catalog. Resolve it under the caller's grant.
func (b *Bridge) namedRemoteSkill(ctx context.Context, p Principal, u *url.URL) (Skill, bool, error) {
	parts := strings.SplitN(strings.TrimPrefix(u.EscapedPath(), "/"), "/", 4)
	if len(parts) < 3 || parts[0] != "upstream" {
		return Skill{}, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !strings.HasPrefix(string(raw), "skill://") || !skillPath(parts[2]) || !strings.HasSuffix(string(raw), "/"+parts[2]+"/SKILL.md") {
		return Skill{}, true, invalid("invalid skill address")
	}
	b.mu.RLock()
	src := b.sources[u.Host]
	r := b.live[u.Host]
	b.mu.RUnlock()
	if r == nil || src.Disabled || !allowed(p, src) || member(src.Hidden, "skill:"+parts[2]) {
		return Skill{}, true, invalid("skill unavailable")
	}
	out, err := external(ctx, r.session, skillsExt, "skills/get", &extParams{URI: string(raw)})
	if err != nil {
		return Skill{}, true, err
	}
	entry, err := json.Marshal(out["skill"])
	if err != nil {
		return Skill{}, true, err
	}
	sk, err := remoteSkill(src, r.session, entry)
	if err != nil {
		return Skill{}, true, err
	}
	if sk.URI != "skill://"+u.Host+"/upstream/"+parts[1]+"/"+parts[2]+"/SKILL.md" {
		return Skill{}, true, invalid("skill unavailable")
	}
	return sk, true, nil
}
func (b *Bridge) findSkill(ctx context.Context, p Principal, uri string) (Skill, error) {
	u, e := url.Parse(uri)
	if e != nil || u.Scheme != "skill" {
		return Skill{}, invalid("invalid skill URI")
	}
	if sk, ok, err := b.namedRemoteSkill(ctx, p, u); ok {
		return sk, err
	}
	l, e := b.skillCatalog(ctx, p, u.Host)
	if e != nil {
		return Skill{}, e
	}
	for _, s := range l {
		// Continue serving previously published hash-based skill addresses.
		if s.remote != nil && strings.HasPrefix(u.Path, "/remote/") {
			old := "skill://" + u.Host + "/remote/" + digest([]byte(s.originRoot + "SKILL.md"))[:16] + "/" + url.PathEscape(fmt.Sprint(s.Front["name"])) + "/"
			canonical := strings.TrimSuffix(s.URI, "SKILL.md")
			s.URI = old + "SKILL.md"
			for i, f := range s.Files {
				key := old + strings.TrimPrefix(f.URI, canonical)
				s.original[key] = s.original[f.URI]
				s.Files[i].URI = key
			}
		}
		if path, ok := strings.CutPrefix(uri, strings.TrimSuffix(s.URI, "SKILL.md")); s.Files == nil && ok && skillPath(path) {
			return s, nil
		}
		for _, f := range s.Files {
			if f.URI == uri {
				return s, nil
			}
		}
	}
	return Skill{}, invalid("skill unavailable")
}
func (b *Bridge) skillTools(v *view) {
	tool(v, "search_skills", "Find authorized skill manifests by source and subject.", func(ctx context.Context, q inputQuery) (any, error) {
		list, e := b.skillCatalog(ctx, v.user, q.Source)
		if e != nil {
			return nil, e
		}
		hits := []searchHit{}
		for _, sk := range list {
			uri, err := url.Parse(sk.URI)
			if err != nil {
				return nil, err
			}
			source := uri.Host
			name := fmt.Sprint(sk.Front["name"])
			hits = append(hits, searchHit{sk.URI, name, fmt.Sprint(sk.Front["description"]), b.aliases(source, "skill:"+name), Object{"skill_id": sk.URI, "frontmatter": sk.Front}})
		}
		return searchPage("skills", hits, q), nil
	})
	type skillInput struct {
		ID   string `json:"skill_id"`
		Path string `json:"path,omitempty"`
	}
	tool(v, "get_skill", "Read the skill instructions or a file relative to the skill directory.", func(ctx context.Context, in skillInput) (any, error) {
		s, e := b.findSkill(ctx, v.user, in.ID)
		if e != nil {
			return nil, e
		}
		uri := s.URI
		if in.Path != "" {
			if !fs.ValidPath(in.Path) {
				return nil, errors.New("invalid path")
			}
			uri = strings.TrimSuffix(uri, "SKILL.md") + in.Path
		}
		data, e := s.read(ctx, uri)
		if e != nil {
			return nil, e
		}
		return Object{"uri": uri, "text": string(data), "manifest": s}, nil
	})
	native(v, "skills/list", func(ctx context.Context, _ *extParams) (Object, error) {
		s, e := b.skillCatalog(ctx, v.user, "")
		return Object{"skills": s, "ttlMs": 0, "cacheScope": "private"}, e
	})
	native(v, "skills/get", func(ctx context.Context, p *extParams) (Object, error) {
		s, e := b.findSkill(ctx, v.user, p.URI)
		if e == nil && s.URI != p.URI {
			return nil, invalid("expected a SKILL.md URI")
		}
		return Object{"skill": s, "ttlMs": 0, "cacheScope": "private"}, e
	})
	native(v, "resources/directory/read", func(ctx context.Context, p *extParams) (Object, error) { return b.skillDirectory(ctx, v.user, p) })
	v.server.AddResourceTemplate(&mcp.ResourceTemplate{Name: "skill-files", URITemplate: "skill://{+path}"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		s, e := b.findSkill(ctx, v.user, req.Params.URI)
		if e != nil {
			return nil, e
		}
		data, e := s.read(ctx, req.Params.URI)
		if e != nil {
			return nil, e
		}
		resource := &mcp.ResourceContents{URI: req.Params.URI, Blob: data}
		if strings.HasSuffix(req.Params.URI, ".md") {
			resource.Text = string(data)
			resource.MIMEType = "text/markdown"
			resource.Blob = nil
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{resource}}, nil
	})
}

func (b *Bridge) skillDirectory(ctx context.Context, p Principal, in *extParams) (Object, error) {
	u, err := url.Parse(in.URI)
	if err != nil || u.Scheme != "skill" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(in.URI, "/") {
		return nil, invalid("invalid skill directory")
	}
	var skills []Skill
	if sk, ok, e := b.namedRemoteSkill(ctx, p, u); ok {
		if e != nil {
			return nil, e
		}
		skills = []Skill{sk}
	} else {
		skills, err = b.skillCatalog(ctx, p, u.Host)
	}
	if err != nil {
		return nil, err
	}
	prefix := in.URI + "/"
	for _, s := range skills {
		root := strings.TrimSuffix(s.URI, "SKILL.md")
		sub, ok := strings.CutPrefix(prefix, root)
		if !ok || sub != "" && !skillPath(strings.TrimSuffix(sub, "/")) {
			continue
		}
		if s.Files == nil {
			up := strings.TrimSuffix(s.originRoot+sub, "/")
			options := s.remote.InitializeResult().Capabilities.Extensions[skillsExt]
			raw, _ := json.Marshal(options)
			var config struct {
				DirectoryRead bool `json:"directoryRead"`
			}
			_ = json.Unmarshal(raw, &config)
			if !config.DirectoryRead {
				return nil, invalid("upstream does not serve directory listings")
			}
			out, err := external(ctx, s.remote, skillsExt, "resources/directory/read", &extParams{URI: up, Cursor: in.Cursor})
			if err != nil {
				return nil, err
			}
			resources, ok := out["resources"].([]any)
			if !ok {
				return nil, invalid("invalid directory listing")
			}
			for _, v := range resources {
				item, _ := v.(map[string]any)
				uri, _ := item["uri"].(string)
				child, ok := strings.CutPrefix(uri, up+"/")
				if !ok || !skillPath(child) || strings.Contains(child, "/") {
					return nil, invalid("invalid directory entry")
				}
				item["uri"] = prefix + child
			}
			return out, nil
		}
		if in.Cursor != nil && in.Cursor != "" {
			return nil, invalid("invalid directory cursor")
		}
		items := []Object{}
		seen := map[string]bool{}
		for _, f := range s.Files {
			rel, ok := strings.CutPrefix(f.URI, prefix)
			if !ok {
				continue
			}
			name, _, dir := strings.Cut(rel, "/")
			if seen[name] {
				continue
			}
			seen[name] = true
			item := Object{"name": name, "uri": prefix + name}
			if dir {
				item["mimeType"] = "inode/directory"
			} else {
				item["size"] = f.Size
			}
			items = append(items, item)
		}
		if len(items) > 0 {
			return Object{"resources": items, "ttlMs": 0, "cacheScope": "private"}, nil
		}
	}
	return nil, invalid("skill directory unavailable")
}
