package bridge

import (
	"context"
	"encoding/json"
	"maps"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var generatedToolAlias = regexp.MustCompile(`^(app|file)_.+_[a-f0-9]{16}$`)
var aliasCharacters = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// Expose the original schema so a file-aware client can recognize x-mcp-file
// before it chooses a file. call_tool remains available for programmatic routing.
func (b *Bridge) publishFileTools(v *view) {
	next := map[string]string{}
	counts := map[string]int{}
	if v.scope == "" {
		for id, r := range b.live {
			if src := b.sources[id]; !src.Disabled && allowed(v.user, src) {
				for n, t := range r.tools {
					if !member(src.Hidden, n) && (len(settings(t.Meta["ui"])) > 0 || appResource(t) != "") {
						counts[n]++
					}
				}
			}
		}
	}

	for source, r := range b.live {
		s := b.sources[source]
		if v.scope != "" && v.scope != source || s.Disabled || !allowed(v.user, s) {
			continue
		}
		opts, files := r.session.InitializeResult().Capabilities.Extensions[filesExt]
		files = files && settings(opts)["revision"] == nil
		for name, original := range r.tools {
			ui := settings(original.Meta["ui"])
			app := len(ui) > 0 || appResource(original) != ""
			if v.scope == "" && !app && !files || member(s.Hidden, name) {
				continue
			}
			id := source + "/" + name
			stem := aliasCharacters.ReplaceAllString(source+"_"+name, "_")
			alias := "file_" + stem[:min(len(stem), 42)] + "_" + digest([]byte(id))[:16]
			t := *original
			if app {
				alias = "app_" + strings.TrimPrefix(alias, "file_")
				if uri := appResource(original); uri != "" {
					ui["resourceUri"] = resourceURI(source, uri)
					t.Meta = maps.Clone(t.Meta)
					t.Meta["ui"] = ui
					if t.Meta["ui/resourceUri"] != nil {
						t.Meta["ui/resourceUri"] = ui["resourceUri"]
					}
				}
			}
			if app && counts[name] == 1 && !v.reserved[name] && !generatedToolAlias.MatchString(name) {
				alias = name
			}
			if v.scope != "" {
				alias = name
				t.Meta = original.Meta
			}
			t.Name = alias
			next[alias] = id
			v.server.AddTool(&t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args Object
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, err
				}
				return b.callUpstream(ctx, v.user, source, name, args, nil)
			})
		}
	}
	for key := range v.fileTools {
		if _, ok := next[key]; !ok {
			v.server.RemoveTools(key)
		}
	}
	v.fileTools = next
}

const uiExt = "io.modelcontextprotocol/ui"

func uiCapabilities() Object { return Object{"mimeTypes": []string{"text/html;profile=mcp-app"}} }
func appResource(tool *mcp.Tool) string {
	ui := settings(tool.Meta["ui"])
	uri, _ := ui["resourceUri"].(string)
	if uri == "" {
		uri, _ = tool.Meta["ui/resourceUri"].(string)
	}
	if !strings.HasPrefix(uri, "ui://") {
		return ""
	}
	return uri
}
