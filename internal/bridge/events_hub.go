package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Request IDs belong to the SDK. Correlate each extension stream before writing
// the request; consume notifications for that request on the same stdio connection.
type streamSink struct {
	ctx    context.Context
	frames chan []byte
}
type streamHub struct {
	pending, active, progress sync.Map
	subMu                     sync.Mutex
	subscriptions             map[string]int
}

func (h *streamHub) stream(ctx context.Context, session *mcp.ClientSession, f Flow, cursor any, frame func([]byte) error) error {
	key := uuid.NewString()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sink := streamSink{ctx, make(chan []byte, 16)}
	h.pending.Store(key, sink)
	defer h.pending.Delete(key)
	done := make(chan error, 1)
	go func() {
		params := &extParams{Name: f.Name, Arguments: f.Filter, Cursor: cursor}
		params.Meta = mcp.Meta{"io.newmcp.bridge/stream": key}
		_, e := external(ctx, session, eventsExt, "events/stream", params)
		done <- e
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-done:
			return e
		case data := <-sink.frames:
			if e := frame(data); e != nil {
				return e
			}
		}
	}
}
func (c upstreamConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == relayMethod {
		var envelope relayParams
		dec := json.NewDecoder(bytes.NewReader(req.Params))
		dec.UseNumber()
		if err := dec.Decode(&envelope); err != nil {
			return err
		}
		meta, _ := envelope.Body["_meta"].(map[string]any)
		for key, value := range envelope.Meta {
			if key != mcp.MetaKeyClientCapabilities {
				meta[key] = value
			}
		}
		copy := *req
		copy.Method = envelope.Method
		raw, err := json.Marshal(envelope.Body)
		if err != nil {
			return err
		}
		copy.Params = raw
		return c.Connection.Write(ctx, &copy)
	}
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "events/stream" {
		var params Object
		if e := json.Unmarshal(req.Params, &params); e != nil {
			return e
		}
		if meta, ok := params["_meta"].(Object); ok {
			key, _ := meta["io.newmcp.bridge/stream"].(string)
			if sink, ok := c.hub.pending.Load(key); ok {
				c.hub.active.Store(req.ID, sink)
				delete(meta, "io.newmcp.bridge/stream")
				data, e := json.Marshal(params)
				if e != nil {
					return e
				}
				copy := *req
				copy.Params = data
				msg = &copy
			}
		}
	}
	return c.Connection.Write(ctx, msg)
}
