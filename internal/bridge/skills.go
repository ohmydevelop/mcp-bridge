package bridge

import (
	"bytes"
	"context"
	_ "embed"
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
type Skill struct {
	URI      string      `json:"uri"`
	Front    Object      `json:"frontmatter"`
	Files    []SkillFile `json:"resources"`
	bytes    map[string][]byte
	original map[string]string
	remote   *mcp.ClientSession
}

const guide = "---\nname: setup-webhook\ndescription: Route an event to an agent automation.\n---\nUse bridge_list_sources, then search_event_types. Select a configured destination and call save_trigger. Check get_trigger for delivery status."

//go:embed improve-trigger.md
var improveTrigger []byte

func localSkill(id, name string, files map[string][]byte) (Skill, error) {
	prefix := "skill://" + id + "/" + name + "/"
	s := Skill{URI: prefix + "SKILL.md", bytes: map[string][]byte{}}
	parts := bytes.SplitN(files["SKILL.md"], []byte("---\n"), 3)
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
				var sk Skill
				if e = json.Unmarshal(entry, &sk); e != nil {
					continue
				}
				if member(s.Hidden, "skill:"+fmt.Sprint(sk.Front["name"])) {
					continue
				}
				original := sk.URI
				prefix := strings.TrimSuffix(original, "SKILL.md")
				if prefix == original {
					continue
				}
				local := "skill://" + s.ID + "/remote/" + digest([]byte(original))[:16] + "/" + url.PathEscape(fmt.Sprint(sk.Front["name"])) + "/"
				sk.URI = local + "SKILL.md"
				sk.remote = r
				sk.original = map[string]string{}
				for i, f := range sk.Files {
					if !strings.HasPrefix(f.URI, prefix) {
						return nil, errors.New("skill manifest contains foreign URI")
					}
					path := strings.TrimPrefix(f.URI, prefix)
					if !fs.ValidPath(path) {
						return nil, errors.New("invalid skill resource path")
					}
					if _, exists := sk.original[local+path]; exists {
						return nil, errors.New("duplicate skill resource")
					}
					sk.Files[i].URI = local + path
					sk.original[local+path] = f.URI
				}
				if _, ok := sk.original[sk.URI]; !ok {
					return nil, errors.New("missing SKILL.md resource")
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
func (sk Skill) read(ctx context.Context, uri string) ([]byte, error) {
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
	return nil, errors.New("unlisted skill file")
}
func (b *Bridge) findSkill(ctx context.Context, p Principal, uri string) (Skill, error) {
	u, e := url.Parse(uri)
	if e != nil || u.Scheme != "skill" {
		return Skill{}, errors.New("invalid skill URI")
	}
	l, e := b.skillCatalog(ctx, p, u.Host)
	if e != nil {
		return Skill{}, e
	}
	for _, s := range l {
		for _, f := range s.Files {
			if f.URI == uri {
				return s, nil
			}
		}
	}
	return Skill{}, errors.New("skill unavailable")
}
func (b *Bridge) skillTools(v *view) {
	tool(v.server, "search_skills", "Find authorized skill manifests by source and subject.", func(ctx context.Context, q inputQuery) (any, error) {
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
	tool(v.server, "get_skill", "Read verified skill content or an explicitly listed relative file.", func(ctx context.Context, in skillInput) (any, error) {
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
			return nil, errors.New("expected a SKILL.md URI")
		}
		return Object{"skill": s, "ttlMs": 0, "cacheScope": "private"}, e
	})
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
