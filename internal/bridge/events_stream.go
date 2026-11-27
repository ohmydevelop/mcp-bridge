package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (b *Bridge) localType(p Principal, name string) (string, string, error) {
	src, n, ok := strings.Cut(name, "__")
	if !ok || !b.visible(p, src) {
		return "", "", errors.New("unknown event")
	}
	if src == "bridge.builtin" && member(builtinEvents, n) {
		return src, n, nil
	}
	b.mu.RLock()
	_, found := b.sources[src].Events[n]
	b.mu.RUnlock()
	if !found {
		return "", "", errors.New("unknown event")
	}
	return src, n, nil
}

type streamRequest struct {
	ID     any       `json:"id"`
	Method string    `json:"method"`
	Params extParams `json:"params"`
}

func (b *Bridge) stream(ctx context.Context, p Principal, r streamRequest, send func(Object) error) error {
	src, name, e := b.localType(p, r.Params.Name)
	if e != nil {
		return e
	}
	if r.Params.Meta[mcp.MetaKeyProtocolVersion] != "2026-07-28" || r.ID == nil {
		return errors.New("invalid modern stream request")
	}
	cursor := r.Params.Cursor
	if cursor == nil {
		state, e := b.pollLocal(p, src, name, nil, nil, 1)
		if e != nil {
			return e
		}
		cursor = state["cursor"]
	}
	notify := func(method string, params Object) error {
		params["_meta"] = Object{"io.modelcontextprotocol/subscriptionId": r.ID}
		return send(Object{"jsonrpc": "2.0", "method": "notifications/events/" + method, "params": params})
	}
	if e = notify("active", Object{"heartbeatMs": 15000, "cursor": cursor, "truncated": false}); e != nil {
		return e
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return send(Object{"jsonrpc": "2.0", "id": r.ID, "result": Object{"resultType": "complete", "_meta": Object{"io.modelcontextprotocol/subscriptionId": r.ID}}})
		case <-heartbeat.C:
			if e = notify("heartbeat", Object{"cursor": cursor}); e != nil {
				return e
			}
		case <-ticker.C:
			state, e := b.pollLocal(p, src, name, r.Params.Arguments, cursor, 100)
			if e != nil {
				return e
			}
			for _, ev := range state["events"].([]Occurrence) {
				if e = notify("event", Object{"name": r.Params.Name, "eventId": ev.ID, "data": ev.Data, "cursor": ev.Cursor, "timestamp": ev.Timestamp}); e != nil {
					return e
				}
			}
			cursor = state["cursor"]
		}
	}
}
func extensionNamed(method string) bool {
	switch method {
	case "events/poll", "events/stream", "events/subscribe", "events/unsubscribe":
		return true
	}
	return false
}
func (b *Bridge) httpMCP(p Principal, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Header.Get("Mcp-Method")
		if extensionNamed(method) {
			var q streamRequest
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
			if err != nil || json.Unmarshal(data, &q) != nil || q.Method != method || q.Params.Name == "" || r.Header.Get("Mcp-Name") != q.Params.Name {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(Object{"jsonrpc": "2.0", "id": q.ID, "error": Object{"code": -32020, "message": "Mcp-Name or Mcp-Method mismatch"}})
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
		}
		if method != "events/stream" {
			next.ServeHTTP(w, r)
			return
		}
		var q streamRequest
		if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q) != nil || q.Method != "events/stream" || q.Params.Name != r.Header.Get("Mcp-Name") || r.Header.Get("MCP-Protocol-Version") != "2026-07-28" {
			http.Error(w, "invalid stream request", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		send := func(obj Object) error {
			data, e := json.Marshal(obj)
			if e != nil {
				return e
			}
			if _, e = fmt.Fprintf(w, "data: %s\n\n", data); e != nil {
				return e
			}
			return http.NewResponseController(w).Flush()
		}
		if e := b.stream(r.Context(), p, q, send); e != nil && r.Context().Err() == nil {
			_ = send(errorFrame(q.ID, e))
		}
	})
}
func errorFrame(id any, e error) Object {
	return Object{"jsonrpc": "2.0", "id": id, "error": Object{"code": -32602, "message": e.Error()}}
}

type eventTransport struct {
	base mcp.Transport
	b    *Bridge
	p    Principal
}

func (t eventTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, e := t.base.Connect(ctx)
	if e != nil {
		return nil, e
	}
	life, end := context.WithCancel(t.b.ctx)
	return &streamConn{Connection: c, b: t.b, p: t.p, life: life, end: end}, nil
}

type streamConn struct {
	mcp.Connection
	b       *Bridge
	p       Principal
	life    context.Context
	end     context.CancelFunc
	streams sync.Map
}

func (c *streamConn) Close() error {
	c.end()
	return c.Connection.Close()
}
func (c *streamConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		msg, e := c.Connection.Read(ctx)
		if e != nil {
			c.end()
			return nil, e
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok {
			return msg, nil
		}
		if req.Method == "notifications/cancelled" {
			var x struct {
				ID any `json:"requestId"`
			}
			_ = json.Unmarshal(req.Params, &x)
			switch x.ID.(type) {
			case string, float64:
			default:
				return nil, errors.New("invalid cancellation id")
			}
			if cancel, ok := c.streams.Load(x.ID); ok {
				cancel.(context.CancelFunc)()
				continue
			}
		}
		if req.Method != "events/stream" {
			return msg, nil
		}
		raw, e := jsonrpc.EncodeMessage(req)
		if e != nil {
			return nil, e
		}
		var q streamRequest
		if e = json.Unmarshal(raw, &q); e != nil {
			return nil, e
		}
		sub, end := context.WithCancel(c.life)
		if _, exists := c.streams.LoadOrStore(q.ID, end); exists {
			end()
			return nil, errors.New("duplicate stream id")
		}
		go func() {
			defer c.streams.Delete(q.ID)
			defer end()
			send := func(obj Object) error {
				raw, e := json.Marshal(obj)
				if e != nil {
					return e
				}
				out, e := jsonrpc.DecodeMessage(raw)
				if e != nil {
					return e
				}
				return c.Connection.Write(c.life, out)
			}
			if e := c.b.stream(sub, c.p, q, send); e != nil && !errors.Is(e, io.EOF) {
				_ = send(errorFrame(q.ID, e))
			}
		}()
	}
}
