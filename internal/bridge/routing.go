package bridge

import (
	"context"
	"errors"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type taskReadScopeKey struct{}

func (b *Bridge) session(p Principal, id string) (*mcp.ClientSession, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	src, ok := b.sources[id]
	r := b.live[id]
	if !ok || !allowed(p, src) || r == nil {
		return nil, errors.New("source unavailable")
	}
	return r.session, nil
}
func (b *Bridge) call(ctx context.Context, p Principal, id, n string, arguments any) (*mcp.CallToolResult, error) {
	if id == "bridge.builtin" {
		return b.runBinding(ctx, p, n, arguments)
	}
	return b.callUpstream(ctx, p, id, n, arguments, nil)
}

func (b *Bridge) callUpstream(ctx context.Context, p Principal, id, n string, arguments any, observe func(string, any, bool)) (*mcp.CallToolResult, error) {
	b.mu.RLock()
	src := b.sources[id]
	r := b.live[id]
	ok := r != nil && allowed(p, src) && r.tools[n] != nil && !member(src.Hidden, n)
	readOnly := ok && r.tools[n].Annotations != nil && r.tools[n].Annotations.ReadOnlyHint
	b.mu.RUnlock()
	if ctx.Value(backgroundKey{}) != nil && !readOnly {
		return nil, errors.New("background tools must be read-only")
	}
	if !ok {
		return nil, errors.New("tool unavailable")
	}
	result, e := r.session.CallTool(ctx, &mcp.CallToolParams{Name: n, Arguments: arguments})
	eligible := readOnly && e == nil && result != nil && !result.IsError
	tool := id + "/" + n
	if observe != nil {
		observe(tool, arguments, eligible)
	}
	if scope, _ := ctx.Value(taskReadScopeKey{}).(string); eligible && scope != "" {
		if err := b.recordTaskRead(ctx, p.Name, scope, tool, arguments); err != nil {
			log.Printf("cannot record scoped tool read: %v", err)
		}
	}
	return result, e
}
