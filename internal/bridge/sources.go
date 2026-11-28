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
	"sync/atomic"
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
	for key, v := range b.scopedViews {
		b.publishResources(v)
		if remove && v.scope == src.ID {
			delete(b.scopedViews, key)
		}
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
	if curRev > 0 && (current.OAuth != nil || src.OAuth != nil) && !canUse(p.Name, current.Owner, nil) {
		return nil, errors.New("source unavailable")
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
	pendingLogin := false
	var next *backend
	if !src.Disabled && config.Type != "package" {
		prior, _ := b.resolve(oldSrc)
		if old != nil && sameConnection(prior, config) {
			next = old
		} else {
			next, e = b.connect(ctx, config)
			pendingLogin = errors.Is(e, loginRequired)
			if pendingLogin && src.OAuth == nil {
				src.OAuth = &sourceOAuth{Mode: "authorization_code"}
				if err := src.OAuth.validate(src); err != nil {
					return nil, err
				}
			}
			if e != nil && !pendingLogin {
				return nil, fmt.Errorf("source probe failed; configuration was not activated")
			}
		}
	}
	for event, tool := range src.Events {
		if strings.TrimSpace(event) == "" || (!pendingLogin && !src.Disabled && tool != "" && (next == nil || next.tools[tool] == nil)) {
			if next != nil && next != old {
				next.session.Close()
			}
			return nil, fmt.Errorf("invalid event or poll tool for %q", event)
		}
	}
	tx, e := b.store.DB.BeginTx(ctx, nil)
	if e == nil {
		defer tx.Rollback()
		e = b.store.PutTx(tx, p.Name, "source", src.ID, src, rev)
		if e == nil && (src.OAuth == nil || oauthKey(src) != oauthKey(current)) {
			_, e = tx.Exec("DELETE FROM oauth_grants WHERE source=?", src.ID)
		}
		if e == nil {
			e = tx.Commit()
		}
	}
	if e != nil {
		if next != nil && next != old {
			next.session.Close()
		}
		return nil, e
	}
	b.replace(src, next, false)
	state := Object{"id": src.ID, "state": "ready", "revision": rev + 1}
	if pendingLogin {
		state["state"] = "authorization_required"
		state["next_tool"] = "bridge_begin_oauth"
	}
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
	r, e := b.connect(ctx, config)
	b.replace(src, r, false)
	if e != nil {
		return Object{"id": src.ID, "state": "connection_failed", "saved": true}, nil
	}
	return Object{"id": src.ID, "state": "ready", "tools": len(r.tools)}, nil
}

type headersTransport struct {
	headers   map[string]string
	challenge *atomic.Bool
}

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
		key := q.Params.Name
		if taskMethod(method) {
			key = q.Params.Task
		}
		r.Header.Set("Mcp-Name", key)
	}
	res, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && h.challenge != nil && (res.StatusCode == 401 || res.StatusCode == 403) && strings.Contains(strings.ToLower(res.Header.Get("WWW-Authenticate")), "bearer") {
		h.challenge.Store(true)
	}
	return res, err
}
func (b *Bridge) connect(ctx context.Context, src Source) (*backend, error) {
	hub := &streamHub{}
	handler, err := b.oauthHandler(src, nil)
	if err != nil {
		return nil, err
	}
	var challenge atomic.Bool
	var transport mcp.Transport
	switch src.Type {
	case "stdio":
		cmd := exec.Command(src.Command, src.Args...)
		cmd.Dir = src.Cwd
		cmd.Env = platform.ChildEnv(false)
		for k, v := range src.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		transport = &mcp.CommandTransport{Command: cmd}
	case "sse":
		transport = &mcp.SSEClientTransport{Endpoint: src.URL, HTTPClient: &http.Client{Transport: headersTransport{headers: src.Headers, challenge: &challenge}, CheckRedirect: rejectRedirect}}
	case "streamable-http":
		t := &mcp.StreamableClientTransport{Endpoint: src.URL, HTTPClient: &http.Client{Transport: headersTransport{headers: src.Headers, challenge: &challenge}, CheckRedirect: rejectRedirect}}
		if handler != nil {
			t.OAuthHandler = handler
		}
		transport = t
	default:
		return nil, errors.New("unsupported transport")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := newClient(b.clientOptions(src, hub))
	session, e := c.Connect(ctx, upstreamTransport{transport, hub}, nil)
	if e != nil {
		if challenge.Load() && src.OAuth == nil && len(src.Headers) == 0 {
			return nil, loginRequired
		}
		return nil, e
	}
	r, err := readCatalog(ctx, session, hub)
	if err != nil {
		session.Close()
		if challenge.Load() && src.OAuth == nil && len(src.Headers) == 0 {
			return nil, loginRequired
		}
	}
	if r != nil {
		r.transportSource = src
		r.oauth = handler
	}
	return r, err
}
func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
