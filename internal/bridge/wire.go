package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"newmcp/mcp-bridge/internal/mcpinfo"
)

// Preserve extension fields that the core SDK's legacy typed tool response omits.
type upstreamTransport struct {
	mcp.Transport
	hub *streamHub
}

func (t upstreamTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, e := t.Transport.Connect(ctx)
	if e != nil {
		return nil, e
	}
	return upstreamConn{c, t.hub}, nil
}

type upstreamConn struct {
	mcp.Connection
	hub *streamHub
}

func (c upstreamConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		msg, e := c.Connection.Read(ctx)
		if n, ok := msg.(*jsonrpc.Request); ok && strings.HasPrefix(n.Method, "notifications/events/") {
			var q struct {
				Meta mcp.Meta `json:"_meta"`
			}
			if err := json.Unmarshal(n.Params, &q); err != nil {
				return nil, err
			}
			id, err := jsonrpc.MakeID(q.Meta["io.modelcontextprotocol/subscriptionId"])
			if err == nil {
				if v, ok := c.hub.active.Load(id); ok {
					sink := v.(streamSink)
					raw, err := jsonrpc.EncodeMessage(n)
					if err != nil {
						return nil, err
					}
					select {
					case <-sink.ctx.Done():
					case sink.frames <- raw:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					continue
				}
			}
		}
		if e == nil {
			if r, ok := msg.(*jsonrpc.Response); ok {
				c.hub.active.Delete(r.ID)

			}
		}
		return msg, e
	}
}
func parseSSE(reader io.Reader, frame func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var data bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				if e := frame(bytes.TrimSuffix(data.Bytes(), []byte("\n"))); e != nil {
					return e
				}
				data.Reset()
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len()+len(rest) > 8<<20 {
				return errors.New("SSE frame too large")
			}
			data.WriteString(strings.TrimPrefix(rest, " "))
			data.WriteByte('\n')
		}
	}
	if e := scanner.Err(); e != nil {
		return e
	}
	if data.Len() > 0 {
		return frame(bytes.TrimSpace(data.Bytes()))
	}
	return nil
}
func modernRPC(ctx context.Context, src Source, oauth *protectedOAuth, method, name string, args Object, frame func([]byte) error) error {
	args["_meta"] = Object{mcp.MetaKeyProtocolVersion: "2026-07-28", mcp.MetaKeyClientCapabilities: Object{"extensions": Object{eventsExt: Object{}, skillsExt: Object{}}}, mcp.MetaKeyClientInfo: mcpinfo.Implementation}
	raw, e := json.Marshal(Object{"jsonrpc": "2.0", "id": 1, "method": method, "params": args})
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", src.URL, bytes.NewReader(raw))
	if e != nil {
		return e
	}
	for k, v := range src.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if err := oauth.attach(req); err != nil {
		return err
	}
	res, e := (&http.Client{CheckRedirect: rejectRedirect}).Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("upstream HTTP %d", res.StatusCode)
	}
	if strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		return parseSSE(res.Body, frame)
	}
	data, e := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if e != nil {
		return e
	}
	if len(data) > 8<<20 {
		return errors.New("upstream response too large")
	}
	return frame(data)
}
