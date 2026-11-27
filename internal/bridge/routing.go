package bridge

import (
	"context"
	"errors"

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
	b.mu.RLock()
	src := b.sources[id]
	r := b.live[id]
	ok := r != nil && allowed(p, src) && r.tools[n] != nil && !member(src.Hidden, n)
	b.mu.RUnlock()
	if !ok {
		return nil, errors.New("tool unavailable")
	}
	return r.session.CallTool(ctx, &mcp.CallToolParams{Name: n, Arguments: arguments})
}
