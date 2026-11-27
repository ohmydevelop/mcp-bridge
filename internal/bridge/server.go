package bridge

import (
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"newmcp/mcp-bridge/internal/mcpinfo"
	"newmcp/mcp-bridge/internal/storage"
)

type inputID struct {
	ID string `json:"id"`
}
type inputQuery struct {
	Query  string `json:"query,omitempty"`
	Source string `json:"source_id,omitempty"`
	Offset int    `json:"offset,omitempty"`
}

func tool[I any](s *mcp.Server, name, desc string, fn func(context.Context, I) (any, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, func(ctx context.Context, _ *mcp.CallToolRequest, in I) (*mcp.CallToolResult, any, error) {
		v, e := fn(ctx, in)
		if e != nil {
			return nil, nil, e
		}
		if r, ok := v.(*mcp.CallToolResult); ok {
			return r, nil, nil
		}
		raw, e := json.Marshal(v)
		if e != nil {
			return nil, nil, e
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}, StructuredContent: v}, nil, nil
	})
}
func page[T any](all []T, start int) ([]T, int) {
	if start < 0 || start > len(all) {
		return []T{}, 0
	}
	end := min(start+50, len(all))
	next := end
	if next == len(all) {
		next = 0
	}
	return all[start:end], next
}
func (b *Bridge) server(p Principal) *mcp.Server {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v := b.views[p]; v != nil {
		return v.server
	}
	if source := b.profiles[p.Name].Producer; source != "" {
		s := mcp.NewServer(mcpinfo.Implementation, nil)
		b.producerTool(s, source)
		return s
	}
	s := mcp.NewServer(mcpinfo.Implementation, serverOptions())
	v := &view{user: p, server: s}
	b.views[p] = v
	tool(s, "bridge_list_sources", "List configured, authorized sources and their connection status.", func(_ context.Context, q inputQuery) (any, error) {
		b.mu.RLock()
		defer b.mu.RUnlock()
		ids := []string{}
		for id, src := range b.sources {
			if allowed(p, src) && strings.Contains(id, q.Query) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		items := []Object{}
		if strings.Contains("bridge.builtin", q.Query) {
			items = append(items, Object{"id": "bridge.builtin", "type": "builtin", "ready": true})
		}
		for _, id := range ids {
			var src Source
			rev, e := b.store.Get("source", id, &src)
			if e != nil {
				return nil, e
			}
			items = append(items, Object{"id": id, "type": src.Type, "package": src.Package, "ready": b.live[id] != nil || !src.Disabled && src.Type == "package" && src.Server == "", "capabilities": b.sourceCapabilities(src), "disabled": src.Disabled, "revision": rev})
		}
		out, n := page(items, q.Offset)
		return Object{"sources": out, "next_offset": n}, nil
	})
	tool(s, "search_tools", "Find a business tool by intent, optionally within one source. Use call_tool with the returned ID and schema.", func(ctx context.Context, q inputQuery) (any, error) { return b.searchTools(p, q) })
	mcp.AddTool(s, &mcp.Tool{Name: "call_tool", Description: "Call a discovered tool using its source-qualified ID. Codex reads use the current turn automatically. Supply task_scope to override."}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		TaskScope string `json:"task_scope,omitempty"`
		ID        string `json:"tool_id"`
		Arguments Object `json:"arguments"`
	}) (*mcp.CallToolResult, any, error) {
		src, n, _ := strings.Cut(in.ID, "/")
		ctx, e := scopedContext(ctx, in.TaskScope, req.Params.Meta)
		if e != nil {
			return nil, nil, e
		}
		r, e := b.call(ctx, p, src, n, in.Arguments)
		return r, nil, e
	})
	tool(s, "bridge_delete_source", "Delete an unused source by ID and current revision.", func(ctx context.Context, in struct {
		ID       string `json:"id"`
		Revision int    `json:"revision"`
	}) (any, error) {
		b.modify.Lock()
		defer b.modify.Unlock()
		flows, e := storage.List[Flow](b.store, "flow")
		if e != nil {
			return nil, e
		}
		for _, f := range flows {
			if f.Source == in.ID {
				return nil, fmt.Errorf("source referenced by flow %s", f.ID)
			}
		}
		defs, e := storage.List[Binding](b.store, "binding")
		if e != nil {
			return nil, e
		}
		for _, def := range defs {
			for _, ref := range def.Uses {
				if strings.HasPrefix(ref, in.ID+"/") {
					return nil, fmt.Errorf("source referenced by tool %s", def.ID)
				}
			}
		}
		if in.Revision < 1 {
			return nil, errors.New("existing revision required")
		}
		if e = b.store.Put(p.Name, "source", in.ID, nil, in.Revision); e != nil {
			return nil, e
		}
		b.replace(Source{ID: in.ID}, nil, true)
		return Object{"removed": in.ID}, nil
	})
	b.lifecycleTools(v)
	b.bindingTools(v)
	b.management(v)
	b.sourceTools(v)
	b.eventTools(v)
	b.skillTools(v)
	if b.registerApp != nil {
		b.registerApp(s)
	}
	b.publishResources(v)
	return s
}
