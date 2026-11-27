package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"newmcp/mcp-bridge/internal/platform"
)

func (b *Bridge) replace(src Source, r *backend, remove bool) {
	b.mu.Lock()
	old := b.live[src.ID]
	delete(b.live, src.ID)
	if r != nil {
		b.live[src.ID] = r
	}
	if remove {
		delete(b.sources, src.ID)
	} else {
		b.sources[src.ID] = src
	}
	for _, v := range b.views {
		b.publishResources(v)
	}
	b.mu.Unlock()
	if old != nil && old != r {
		old.session.Close()
	}
}
func (b *Bridge) saveSource(ctx context.Context, p Principal, src Source, rev int) (any, error) {
	if !identifier.MatchString(src.ID) || rev < 0 {
		return nil, errors.New("invalid source id or revision")
	}
	b.modify.Lock()
	defer b.modify.Unlock()
	var current Source
	curRev, err := b.store.Get("source", src.ID, &current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if curRev != rev {
		return nil, errors.New("revision conflict")
	}
	src.Owner = current.Owner
	if curRev == 0 {
		src.Owner = p.Name
	}
	config, e := b.resolve(src)
	if e != nil {
		return nil, e
	}
	b.mu.RLock()
	oldSrc := b.sources[src.ID]
	old := b.live[src.ID]
	b.mu.RUnlock()
	var next *backend
	if !src.Disabled && config.Type != "package" {
		prior, _ := b.resolve(oldSrc)
		if old != nil && sameConnection(prior, config) {
			next = old
		} else {
			next, e = connect(ctx, config)
			if e != nil {
				return nil, fmt.Errorf("source probe failed; configuration was not activated")
			}
		}
	}
	for event, tool := range src.Events {
		if strings.TrimSpace(event) == "" || (!src.Disabled && tool != "" && (next == nil || next.tools[tool] == nil)) {
			if next != nil && next != old {
				next.session.Close()
			}
			return nil, fmt.Errorf("invalid event or poll tool for %q", event)
		}
	}
	if e = b.store.Put(p.Name, "source", src.ID, src, rev); e != nil {
		if next != nil && next != old {
			next.session.Close()
		}
		return nil, e
	}
	b.replace(src, next, false)
	state := Object{"id": src.ID, "state": "ready", "revision": rev + 1}
	if src.Disabled {
		state["state"] = "disabled"
	}
	if e == nil && rev == 0 {
		e = b.appendEvents(ctx, "bridge.builtin", []Occurrence{{ID: uuid.NewString(), Name: "source.added", Data: Object{"source_id": src.ID}}}, "", nil)
	}
	return state, e
}
func (b *Bridge) reconcile(ctx context.Context, src Source) (any, error) {
	if src.Disabled {
		b.replace(src, nil, false)
		return Object{"id": src.ID, "state": "disabled"}, nil
	}
	config, e := b.resolve(src)
	if e != nil {
		b.replace(src, nil, false)
		return nil, e
	}
	if config.Type == "package" {
		b.replace(src, nil, false)
		return Object{"id": src.ID, "state": "ready", "tools": 0}, nil
	}
	r, e := connect(ctx, config)
	b.replace(src, r, false)
	if e != nil {
		return Object{"id": src.ID, "state": "connection_failed", "saved": true}, nil
	}
	return Object{"id": src.ID, "state": "ready", "tools": len(r.tools)}, nil
}

type headersTransport struct{ headers map[string]string }

func (h headersTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	if method := r.Header.Get("Mcp-Method"); extensionNamed(method) && req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer body.Close()
		var q streamRequest
		if err := json.NewDecoder(body).Decode(&q); err != nil {
			return nil, err
		}
		r.Header.Set("Mcp-Name", q.Params.Name)
	}
	return http.DefaultTransport.RoundTrip(r)
}
func connect(ctx context.Context, src Source) (*backend, error) {
	hub := &streamHub{}
	var transport mcp.Transport
	switch src.Type {
	case "stdio":
		cmd := exec.Command(src.Command, src.Args...)
		cmd.Dir = src.Cwd
		cmd.Env = platform.ChildEnv(false)
		for k, v := range src.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		transport = upstreamTransport{&mcp.CommandTransport{Command: cmd}, hub}
	case "sse":
		transport = &mcp.SSEClientTransport{Endpoint: src.URL, HTTPClient: &http.Client{Transport: headersTransport{src.Headers}, CheckRedirect: rejectRedirect}}
	case "streamable-http":
		transport = &mcp.StreamableClientTransport{Endpoint: src.URL, HTTPClient: &http.Client{Transport: headersTransport{src.Headers}, CheckRedirect: rejectRedirect}}
	default:
		return nil, errors.New("unsupported transport")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := newClient()
	session, e := c.Connect(ctx, transport, nil)
	if e != nil {
		return nil, e
	}
	r, err := readCatalog(ctx, session, hub)
	if err != nil {
		session.Close()
	}
	return r, err
}
func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
