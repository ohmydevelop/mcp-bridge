package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"newmcp/mcp-bridge/internal/mcpinfo"
)

const skillsExt = "io.modelcontextprotocol/skills"
const eventsExt = "io.modelcontextprotocol/events"

type extParams struct {
	mcp.ParamsBase
	Delivery  *callback `json:"delivery,omitempty"`
	TTL       *int64    `json:"ttlMs,omitempty"`
	URI       string    `json:"uri,omitempty"`
	Name      string    `json:"name,omitempty"`
	Cursor    any       `json:"cursor,omitempty"`
	Arguments Object    `json:"arguments,omitempty"`
	MaxEvents int       `json:"maxEvents,omitempty"`
}
type extResult struct {
	mcp.ResultBase
	Fields Object
}

func (r *extResult) MarshalJSON() ([]byte, error)   { return json.Marshal(r.Fields) }
func (r *extResult) UnmarshalJSON(raw []byte) error { return json.Unmarshal(raw, &r.Fields) }
func complete(obj Object) *extResult                { obj["resultType"] = "complete"; return &extResult{Fields: obj} }
func serverOptions() *mcp.ServerOptions {
	caps := &mcp.ServerCapabilities{}
	caps.AddExtension(skillsExt, Object{})
	caps.AddExtension(eventsExt, Object{})
	return &mcp.ServerOptions{Capabilities: caps, SetCacheable: func(_ context.Context, _ mcp.Request, c *mcp.Cacheable) { c.CacheScope = "private"; c.TTLMs = 0 }}
}
func newClient() *mcp.Client {
	caps := &mcp.ClientCapabilities{}
	caps.AddExtension(skillsExt, Object{})
	caps.AddExtension(eventsExt, Object{})
	c := mcp.NewClient(mcpinfo.Implementation, &mcp.ClientOptions{Capabilities: caps, MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}})
	for _, m := range []string{"skills/list", "skills/get", "events/list", "events/poll", "events/stream", "events/subscribe", "events/unsubscribe"} {
		if e := mcp.AddSendingCustomMethod[*extParams, *extResult](c, m); e != nil {
			panic(e)
		}
	}
	return c
}
func external(ctx context.Context, s *mcp.ClientSession, ext, method string, args *extParams) (Object, error) {
	caps := s.InitializeResult().Capabilities
	if _, ok := caps.Extensions[ext]; !ok {
		return nil, fmt.Errorf("source does not advertise %s", ext)
	}
	if args.Cursor == nil && (method == "events/poll" || method == "events/stream" || method == "events/subscribe") {
		args.Cursor = json.RawMessage("null")
	}
	r, e := mcp.CallCustomMethod[*extParams, *extResult](ctx, s, method, args)
	if e != nil {
		return nil, e
	}
	return r.Fields, nil
}
func native(v *view, method string, fn func(context.Context, *extParams) (Object, error)) {
	e := mcp.AddReceivingCustomMethod(v.server, method, func(ctx context.Context, _ *mcp.ServerSession, p *extParams) (*extResult, error) {
		if p.Meta["io.modelcontextprotocol/protocolVersion"] != "2026-07-28" {
			return nil, errors.New("extension requires 2026-07-28")
		}
		out, e := fn(ctx, p)
		if e != nil {
			return nil, e
		}
		return complete(out), nil
	})
	if e != nil {
		panic(e)
	}
}
