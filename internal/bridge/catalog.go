package bridge

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/yosida95/uritemplate/v3"
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
func readCatalog(ctx context.Context, session *mcp.ClientSession, hub *streamHub) (*backend, error) {
	next := &backend{hub: hub, session: session, tools: map[string]*mcp.Tool{}}
	cap := session.InitializeResult().Capabilities
	if cap == nil {
		return nil, errors.New("missing server capabilities")
	}
	if cap.Tools != nil {
		var params *mcp.ListToolsParams
		if session.InitializeResult().ProtocolVersion == modernVersion {
			params = &mcp.ListToolsParams{}
			extensions := Object{uiExt: uiCapabilities(), tasksExt: Object{}, skillsExt: Object{}, eventsExt: Object{}}
			if opts, ok := cap.Extensions[filesExt]; ok && settings(opts)["revision"] == nil {
				extensions[filesExt] = Object{"upload": Object{}, "download": Object{}}
			}
			params.Meta = mcp.Meta{mcp.MetaKeyClientCapabilities: Object{"extensions": extensions}}
		}
		for t, e := range session.Tools(ctx, params) {
			if e != nil {
				return nil, e
			}
			next.tools[t.Name] = t
		}
	}
	if cap.Resources != nil {
		for t, err := range session.ResourceTemplates(ctx, nil) {
			if err != nil {
				var rpc *jsonrpc.Error
				if errors.As(err, &rpc) && rpc.Code == -32601 {
					break
				}
				return nil, err
			}
			if _, err := uritemplate.New(t.URITemplate); err != nil {
				return nil, errors.New("invalid upstream URI template")
			}
			next.templates = append(next.templates, t)
		}
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
	next, err := readCatalog(ctx, r.session, r.hub)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.live[src.ID] != r || (reflect.DeepEqual(r.templates, next.templates) && reflect.DeepEqual(r.tools, next.tools) && reflect.DeepEqual(r.resources, next.resources) && reflect.DeepEqual(r.prompts, next.prompts)) {
		return nil
	}
	next.transportSource = r.transportSource
	next.oauth = r.oauth
	b.live[src.ID] = next
	for _, v := range b.scopedViews {
		b.publishResources(v)
	}
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
	b.publishFileTools(v)
	var uris, prompts, templates []string
	for id, r := range b.live {
		if !allowed(v.user, b.sources[id]) || v.scope != "" && v.scope != id {
			continue
		}
		resources := append([]*mcp.Resource(nil), r.resources...)
		known := map[string]bool{}
		for _, res := range resources {
			known[res.URI] = true
		}
		for name, t := range r.tools {
			uri := appResource(t)
			if uri != "" && !known[uri] && !member(b.sources[id].Hidden, name) {
				known[uri] = true
				resources = append(resources, &mcp.Resource{Name: name, URI: uri, MIMEType: "text/html;profile=mcp-app"})
			}
		}
		for _, res := range resources {
			item := *res
			if v.scope == "" {
				item.URI = resourceURI(id, res.URI)
			}
			uris = append(uris, item.URI)
			v.server.AddResource(&item, func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				s, e := b.session(v.user, id)
				if e != nil {
					return nil, e
				}
				out, e := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: res.URI})
				if e == nil && v.scope == "" {
					for _, c := range out.Contents {
						c.URI = resourceURI(id, c.URI)
					}
				}
				return out, e
			})
		}
		for _, t := range r.templates {
			item := *t
			prefix := templatePrefix(id, t.URITemplate)
			item.URITemplate = prefix + t.URITemplate
			templates = append(templates, item.URITemplate)
			v.server.AddResourceTemplate(&item, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				session, err := b.session(v.user, id)
				if err != nil {
					return nil, err
				}
				uri, ok := strings.CutPrefix(req.Params.URI, prefix)
				if !ok {
					return nil, invalid("unknown template")
				}
				out, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
				if err == nil {
					for _, c := range out.Contents {
						if c.URI == uri {
							c.URI = req.Params.URI
						} else {
							c.URI = resourceURI(id, c.URI)
						}
					}
				}
				return out, err
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
	for _, t := range v.templates {
		if !member(templates, t) {
			v.server.RemoveResourceTemplates(t)
		}
	}
	v.templates = templates
	v.uris = uris
	v.prompts = prompts
}

func templatePrefix(source, template string) string {
	return "mcp-template://bridge/" + source + "/" + digest([]byte(template))[:16] + "/"
}

func (b *Bridge) completeReference(ctx context.Context, p Principal, params *mcp.CompleteParams) (*mcp.CompleteResult, error) {
	if params == nil || params.Ref == nil {
		return nil, invalid("missing completion reference")
	}
	in := *params
	ref := *params.Ref
	in.Ref = &ref
	in.Meta = nil
	var session *mcp.ClientSession
	b.mu.RLock()
	for id, r := range b.live {
		src := b.sources[id]
		if src.Disabled || !allowed(p, src) || r.session.InitializeResult().Capabilities.Completions == nil {
			continue
		}
		if ref.Type == "ref/prompt" {
			for _, prompt := range r.prompts {
				if id+"__"+prompt.Name == ref.Name {
					ref.Name = prompt.Name
					session = r.session
					break
				}
			}
		} else {
			for _, t := range r.templates {
				if templatePrefix(id, t.URITemplate)+t.URITemplate == ref.URI {
					ref.URI = t.URITemplate
					session = r.session
					break
				}
			}
		}
		if session != nil {
			break
		}
	}
	b.mu.RUnlock()
	if session == nil {
		return nil, invalid("completion unavailable for reference")
	}
	return session.Complete(ctx, &in)
}

type relayProgressKey struct{}

func (b *Bridge) clientOptions(src Source, hub *streamHub) *mcp.ClientOptions {
	changed := func() {
		select {
		case b.catalogChanged <- struct{}{}:
		default:
		}
	}
	return &mcp.ClientOptions{
		ToolListChangedHandler:     func(context.Context, *mcp.ToolListChangedRequest) { changed() },
		PromptListChangedHandler:   func(context.Context, *mcp.PromptListChangedRequest) { changed() },
		ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) { changed() },
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			id, ok := req.Params.ProgressToken.(string)
			if !ok {
				return
			}
			if sink, ok := hub.progress.Load(id); ok {
				sink.(func(*mcp.ProgressNotificationParams))(req.Params)
			}
		},
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			b.mu.RLock()
			r := b.live[src.ID]
			uris := []string{resourceURI(src.ID, req.Params.URI)}
			var servers []*mcp.Server
			if r != nil && r.session == req.Session {
				for _, t := range r.templates {
					if templateMatches(t.URITemplate, req.Params.URI) {
						uris = append(uris, templatePrefix(src.ID, t.URITemplate)+req.Params.URI)
					}
				}
				for p, v := range b.views {
					if allowed(p, b.sources[src.ID]) {
						servers = append(servers, v.server)
					}
				}
			}
			b.mu.RUnlock()
			for _, s := range servers {
				for _, uri := range uris {
					_ = s.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: uri})
				}
			}
		},
	}
}
func templateMatches(pattern, uri string) bool {
	t, err := uritemplate.New(pattern)
	return err == nil && t.Match(uri) != nil
}

func (b *Bridge) resourceSubscription(ctx context.Context, p Principal, uri string, subscribe bool) error {
	b.mu.RLock()
	var source *backend
	var remote string
	for id, r := range b.live {
		if !allowed(p, b.sources[id]) || r.session.InitializeResult().Capabilities.Resources == nil || !r.session.InitializeResult().Capabilities.Resources.Subscribe {
			continue
		}
		for _, item := range r.resources {
			if resourceURI(id, item.URI) == uri {
				source = r
				remote = item.URI
				break
			}
		}
		if source == nil {
			for _, t := range r.templates {
				candidate, ok := strings.CutPrefix(uri, templatePrefix(id, t.URITemplate))
				if ok && templateMatches(t.URITemplate, candidate) {
					source, remote = r, candidate
					break
				}
			}
		}
		if source != nil {
			break
		}
	}
	b.mu.RUnlock()
	if source == nil {
		return invalid("resource is unavailable for subscription")
	}
	h := source.hub
	h.subMu.Lock()
	defer h.subMu.Unlock()
	if h.subscriptions == nil {
		h.subscriptions = map[string]int{}
	}
	n := h.subscriptions[remote]
	if subscribe {
		if n == 0 {
			if err := source.session.Subscribe(ctx, &mcp.SubscribeParams{URI: remote}); err != nil {
				return err
			}
		}
		h.subscriptions[remote] = n + 1
	} else if n > 1 {
		h.subscriptions[remote] = n - 1
	} else if n == 1 {
		if err := source.session.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: remote}); err != nil {
			return err
		}
		delete(h.subscriptions, remote)
	}
	return nil
}
