package bridge

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"newmcp/mcp-bridge/internal/storage"
)

func resourceURI(source, uri string) string {
	scheme := "mcp-resource"
	if strings.HasPrefix(uri, "ui://") {
		scheme = "ui"
	}
	return scheme + "://mcp-bridge-source/" + source + "/" + base64.RawURLEncoding.EncodeToString([]byte(uri))
}
func readCatalog(ctx context.Context, session *mcp.ClientSession) (*backend, error) {
	next := &backend{session: session, tools: map[string]*mcp.Tool{}}
	cap := session.InitializeResult().Capabilities
	if cap == nil {
		return nil, errors.New("missing server capabilities")
	}
	if cap.Tools != nil {
		for t, e := range session.Tools(ctx, nil) {
			if e != nil {
				return nil, e
			}
			next.tools[t.Name] = t
		}
	}
	if cap.Resources != nil {
		for res, e := range session.Resources(ctx, nil) {
			if e != nil {
				return nil, e
			}
			next.resources = append(next.resources, res)
		}
	}
	if cap.Prompts != nil {
		for pr, e := range session.Prompts(ctx, nil) {
			if e != nil {
				return nil, e
			}
			next.prompts = append(next.prompts, pr)
		}
	}
	return next, nil
}
func (b *Bridge) refreshCatalog(ctx context.Context, src Source, r *backend) error {
	next, err := readCatalog(ctx, r.session)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.live[src.ID] != r || (reflect.DeepEqual(r.tools, next.tools) && reflect.DeepEqual(r.resources, next.resources) && reflect.DeepEqual(r.prompts, next.prompts)) {
		return nil
	}
	b.live[src.ID] = next
	for _, v := range b.views {
		b.publishResources(v)
	}
	return nil
}
func (b *Bridge) syncSources(ctx context.Context) {
	b.modify.Lock()
	defer b.modify.Unlock()
	sources, e := storage.List[Source](b.store, "source")
	if e != nil {
		return
	}
	for _, s := range sources {
		if ctx.Err() != nil {
			return
		}
		if s.Disabled || (s.Type == "package" && s.Server == "") {
			continue
		}
		b.mu.RLock()
		r := b.live[s.ID]
		b.mu.RUnlock()
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		if r == nil {
			_, _ = b.reconcile(probe, s)
		} else if e := b.refreshCatalog(probe, s, r); e != nil {
			_, _ = b.reconcile(probe, s)
		}
		cancel()
	}
}
func (b *Bridge) publishResources(v *view) {
	var uris, prompts []string
	for id, r := range b.live {
		if !allowed(v.user, b.sources[id]) {
			continue
		}
		for _, res := range r.resources {
			item := *res
			item.URI = resourceURI(id, res.URI)
			uris = append(uris, item.URI)
			v.server.AddResource(&item, func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				s, e := b.session(v.user, id)
				if e != nil {
					return nil, e
				}
				out, e := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: res.URI})
				if e == nil {
					for _, c := range out.Contents {
						c.URI = resourceURI(id, c.URI)
					}
				}
				return out, e
			})
		}
		for _, pr := range r.prompts {
			item := *pr
			item.Name = id + "__" + pr.Name
			prompts = append(prompts, item.Name)
			v.server.AddPrompt(&item, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				s, e := b.session(v.user, id)
				if e != nil {
					return nil, e
				}
				return s.GetPrompt(ctx, &mcp.GetPromptParams{Name: pr.Name, Arguments: req.Params.Arguments})
			})
		}
	}
	for _, id := range v.uris {
		if !member(uris, id) {
			v.server.RemoveResources(id)
		}
	}
	for _, n := range v.prompts {
		if !member(prompts, n) {
			v.server.RemovePrompts(n)
		}
	}
	v.uris = uris
	v.prompts = prompts
}
