package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	js "github.com/santhosh-tekuri/jsonschema/v6"

	"newmcp/mcp-bridge/internal/mcpinfo"
	"newmcp/mcp-bridge/internal/pyrunner"
	"newmcp/mcp-bridge/internal/storage"
)

type PythonScript struct {
	Script string   `json:"script"`
	Uses   []string `json:"uses"`
}

type Binding struct {
	PythonScript
	Runtime string   `json:"runtime,omitempty"`
	Aliases []string `json:"aliases,omitempty"`

	ID          string `json:"name"`
	Description string `json:"description"`
	Schema      Object `json:"input_schema"`
}

func (b *Bridge) bindingAllowed(p Principal, def Binding) bool {
	for _, ref := range def.Uses {
		id, n, _ := strings.Cut(ref, "/")
		src, ok := b.sources[id]
		if !ok || !allowed(p, src) || src.Disabled || member(src.Hidden, n) {
			return false
		}
	}
	return true
}
func inputSchema(doc Object) (*js.Schema, error) {
	c := js.NewCompiler()
	if doc["type"] != "object" {
		return nil, errors.New("expected object input schema")
	}
	if e := c.AddResource("urn:bridge:tool", doc); e != nil {
		return nil, e
	}
	return c.Compile("urn:bridge:tool")
}
func (b *Bridge) runBinding(ctx context.Context, p Principal, id string, args any) (*mcp.CallToolResult, error) {
	var def Binding
	revision, e := b.store.Get("binding", id, &def)
	if e != nil {
		return nil, errors.New("binding unavailable")
	}
	b.mu.RLock()
	ok := b.bindingAllowed(p, def)
	b.mu.RUnlock()
	if !ok {
		return nil, errors.New("binding unavailable")
	}
	raw, e := json.Marshal(args)
	if e != nil {
		return nil, e
	}
	var input Object
	if e = json.Unmarshal(raw, &input); e != nil {
		return nil, e
	}
	schema, e := inputSchema(def.Schema)
	if e != nil {
		return nil, e
	}
	if e = schema.Validate(input); e != nil {
		return nil, e
	}
	return b.executePython(ctx, p, def.PythonScript, input, "bridge.builtin/"+id, revision)
}

func (b *Bridge) executePython(ctx context.Context, p Principal, script PythonScript, input Object, toolID string, revision int) (*mcp.CallToolResult, error) {
	metrics := &bindingReads{}
	if rec, _ := ctx.Value(backgroundKey{}).(*executionRecord); rec == nil || !rec.Offline {
		defer b.reportReads(ctx, toolID, revision, time.Now(), metrics, script.Script)
	}
	val, err := pyrunner.Run(ctx, script.Script, input, false, func(ctx context.Context, ref string, args map[string]any) (any, error) {
		if !member(script.Uses, ref) {
			return nil, errors.New("undeclared Python tool")
		}
		if rec, ok := ctx.Value(backgroundKey{}).(*executionRecord); ok && rec.Offline {
			return rec.read(ref, args)
		}
		src, name, _ := strings.Cut(ref, "/")
		r, err := b.callUpstream(ctx, p, src, name, args, metrics.record)
		if err != nil {
			return nil, err
		}
		if r.IsError {
			return nil, errors.New("upstream tool failed")
		}
		var value any = r.StructuredContent
		if value == nil {
			value = r.Content
		}
		if rec, ok := ctx.Value(backgroundKey{}).(*executionRecord); ok {
			rec.record(ref, args, value)
		}
		return value, nil
	})
	if err != nil {
		return nil, err
	}
	return bindingResult(val)
}
func bindingResult(val any) (*mcp.CallToolResult, error) {
	obj := Object{"result": val}
	raw, e := json.Marshal(obj)
	if e != nil {
		return nil, e
	}
	return &mcp.CallToolResult{StructuredContent: obj, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
}
func (b *Bridge) validateScript(p Principal, script PythonScript) error {
	if script.Script == "" || len(script.Script) > 128<<10 {
		return errors.New("expected a Python script (up to 128 KiB) and uses")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ref := range script.Uses {
		src, n, _ := strings.Cut(ref, "/")
		r := b.live[src]
		s := b.sources[src]
		if r == nil || src == "bridge.builtin" || r.tools[n] == nil || s.Disabled || !allowed(p, s) || member(s.Hidden, n) {
			return fmt.Errorf("unavailable tool %s", ref)
		}
	}
	return nil
}

func (b *Bridge) bindingTools(v *view) {
	tool(v.server, "bridge_get_tool", "Read a saved Python tool definition by tool_id and optional version.", func(_ context.Context, in toolVersion) (any, error) {
		def, rev, err := b.readVersion(in, v.user)
		if err != nil {
			return nil, err
		}
		return Object{"tool_id": in.ID, "revision": rev, "definition": def}, nil
	})
	mcp.AddTool(v.server, &mcp.Tool{Name: mcpinfo.ExecuteScript, Description: "Run a one-off Python script without publishing it. Define run(args, ctx), declare upstream tool IDs in uses, and return a JSON value."}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		PythonScript
		Arguments Object `json:"arguments"`
		TaskScope string `json:"task_scope,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		ctx, err := scopedContext(ctx, in.TaskScope, req.Params.Meta)
		if err != nil {
			return nil, nil, err
		}
		if err = b.validateScript(v.user, in.PythonScript); err != nil {
			return nil, nil, err
		}
		result, err := b.executePython(ctx, v.user, in.PythonScript, in.Arguments, mcpinfo.ExecuteScript, 0)
		return result, nil, err
	})
	tool(v.server, "bridge_save_tool", "Publish or preview a custom business tool. Supply a Python script and its allowed upstream tools (uses). On verified promotion, pass origin_delivery_id to notify the triggering Codex thread.", func(ctx context.Context, in struct {
		Binding
		OriginDelivery string `json:"origin_delivery_id,omitempty"`
		Revision       int    `json:"revision,omitempty"`
		Dry            bool   `json:"dry_run,omitempty"`
	}) (any, error) {
		def := in.Binding
		if !mcpinfo.ValidToolName(def.ID) || strings.TrimSpace(def.Description) == "" {
			return nil, errors.New("name and description required")
		}
		if def.Runtime != "" && def.Runtime != "python" {
			return nil, errors.New("expected python runtime")
		}
		def.Runtime = "python"
		if _, e := inputSchema(def.Schema); e != nil {
			return nil, e
		}
		b.modify.Lock()
		defer b.modify.Unlock()
		if err := b.validateScript(v.user, def.PythonScript); err != nil {
			return nil, err
		}
		if _, err := pyrunner.Run(ctx, def.Script, nil, true, nil); err != nil {
			return nil, err
		}
		if in.Dry {
			return Object{"valid": true}, nil
		}
		var e error
		if in.OriginDelivery != "" {
			e = b.publishForTrigger(ctx, v.user.Name, def, in.Revision, in.OriginDelivery)
		} else {
			e = b.store.Put(v.user.Name, "binding", def.ID, def, in.Revision)
		}
		if e != nil {
			return nil, e
		}
		saved, err := b.store.Get("binding", def.ID, &def)
		if err != nil {
			return nil, err
		}
		return Object{"tool_id": "bridge.builtin/" + def.ID, "revision": saved}, nil
	})
	tool(v.server, "bridge_delete_tool", "Delete a published custom tool definition by ID and current revision.", func(_ context.Context, in sourceRevision) (any, error) {
		if in.Revision < 1 {
			return nil, errors.New("existing revision required")
		}
		b.modify.Lock()
		defer b.modify.Unlock()
		id := strings.TrimPrefix(in.ID, "bridge.builtin/")
		flows, err := storage.List[Flow](b.store, "flow")
		if err != nil {
			return nil, err
		}
		for _, f := range flows {
			if f.Handler != nil && f.Handler.ID == "bridge.builtin/"+id {
				return nil, fmt.Errorf("tool referenced by trigger %s", f.ID)
			}
		}
		if e := b.store.Put(v.user.Name, "binding", id, nil, in.Revision); e != nil {
			return nil, e
		}
		return Object{"unpublished": in.ID}, nil
	})
}
