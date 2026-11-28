package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"maps"
	"math/big"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	tasksExt      = "io.modelcontextprotocol/tasks"
	filesExt      = "io.modelcontextprotocol/files"
	relayMethod   = "bridge/internal-forward"
	modernVersion = "2026-07-28"
)

// An internal SDK method preserves extension-defined result variants. The transport
// replaces this envelope before writing to either the stdio or HTTP connection.
type relayParams struct {
	mcp.ParamsBase
	Method string `json:"method"`
	Body   Object `json:"body"`
}

type taskParams struct {
	mcp.ParamsBase
	Task      string          `json:"taskId"`
	Responses json.RawMessage `json:"inputResponses,omitempty"`
}

type routeHandle struct {
	Uploaded  bool `json:",omitempty"`
	Source    string
	Tool      string
	Origin    string
	Value     string
	Arguments string `json:",omitempty"`
	Expires   int64  `json:",omitempty"`
}

func (b *Bridge) encodeRoute(p Principal, kind string, h routeHandle) (string, error) {
	data, err := b.store.Seal("relay/"+kind, p.Name, h)
	return base64.RawURLEncoding.EncodeToString(data), err
}
func (b *Bridge) decodeRoute(p Principal, kind, token string) (routeHandle, error) {
	var h routeHandle
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || token == "" || b.store.Decode("relay/"+kind, p.Name, data, &h) != nil || h.Expires != 0 && h.Expires <= time.Now().Unix() {
		return h, invalid("invalid or expired " + kind)
	}
	return h, nil
}
func invalid(msg string) error { return &jsonrpc.Error{Code: -32602, Message: msg} }
func missingCapability(ext string, options Object) error {
	data, _ := json.Marshal(Object{"requiredCapabilities": Object{"extensions": Object{ext: options}}})
	return &jsonrpc.Error{Code: -32021, Message: "Missing required client capability", Data: data}
}
func capabilityError(msg string) error { return &jsonrpc.Error{Code: -32021, Message: msg} }

func capabilities(meta mcp.Meta) Object {
	raw, _ := json.Marshal(meta[mcp.MetaKeyClientCapabilities])
	var obj Object
	_ = json.Unmarshal(raw, &obj)
	return obj
}
func extension(caps Object, name string) bool {
	exts, _ := caps["extensions"].(map[string]any)
	_, ok := exts[name].(map[string]any)
	return ok
}

func (b *Bridge) relayTarget(p Principal, source, name string) (*backend, routeHandle, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s := b.sources[source]
	r := b.live[source]
	if r == nil || s.Disabled || !allowed(p, s) || r.tools[name] == nil || member(s.Hidden, name) {
		return nil, routeHandle{}, invalid("tool unavailable")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, routeHandle{}, err
	}
	return r, routeHandle{Source: source, Tool: name, Origin: digest(raw)}, nil
}
func forward(ctx context.Context, r *backend, method string, body Object, meta mcp.Meta) (Object, error) {
	// Advertise only capabilities of the caller who will actually answer.
	caps := capabilities(meta)
	if exts, ok := caps["extensions"].(map[string]any); ok {
		offered := r.session.InitializeResult().Capabilities.Extensions
		for key := range exts {
			if _, ok := offered[key]; !ok {
				delete(exts, key)
			}
		}
		if f, ok := exts[filesExt].(map[string]any); ok {
			peer := settings(offered[filesExt])
			if f["revision"] != nil || peer["revision"] != nil {
				delete(exts, filesExt)
			} else {
				opts := Object{}
				if f["upload"] != nil && peer["upload"] != nil {
					opts["upload"] = Object{}
				}
				if f["download"] != nil {
					opts["download"] = Object{}
				}
				exts[filesExt] = opts
			}
		}
	}
	outMeta := Object{mcp.MetaKeyClientCapabilities: caps}
	if tok, ok := ctx.Value(relayProgressKey{}).(string); ok {
		outMeta["progressToken"] = tok
	}
	body["_meta"] = outMeta
	out, err := mcp.CallCustomMethod[*relayParams, *upstreamResult](ctx, r.session, relayMethod, &relayParams{Method: method, Body: body})
	if err != nil {
		return nil, err
	}
	return out.Fields, nil
}

func (b *Bridge) relayTool(ctx context.Context, p Principal, scope string, r *backend, h routeHandle, args json.RawMessage, req *mcp.CallToolRequest) (mcp.Result, error) {
	if token := req.Params.GetProgressToken(); token != nil && r.hub != nil {
		id := uuid.NewString()
		r.hub.progress.Store(id, func(params *mcp.ProgressNotificationParams) {
			copy := *params
			copy.ProgressToken = token
			copy.Meta = nil
			_ = req.Session.NotifyProgress(ctx, &copy)
		})
		defer r.hub.progress.Delete(id)
		ctx = context.WithValue(ctx, relayProgressKey{}, id)
	}
	body := Object{"name": h.Tool, "arguments": args}
	// Bind continuations to the authenticated caller, actual source and arguments.
	var canonical any
	decoder := json.NewDecoder(strings.NewReader(string(args)))
	decoder.UseNumber()
	if err := decoder.Decode(&canonical); err != nil {
		return nil, invalid("invalid arguments")
	}
	var outer any
	if len(req.Params.Arguments) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(req.Params.Arguments)))
		dec.UseNumber()
		if err := dec.Decode(&outer); err != nil {
			return nil, invalid("invalid arguments")
		}
	}
	canon, _ := json.Marshal([]any{req.Params.Name, normalizedNumbers(outer)})
	h.Arguments = digest(canon)
	if token := req.Params.RequestState; token != "" {
		saved, err := b.decodeRoute(p, "continuation", token)
		if err != nil || saved.Origin != h.Origin || saved.Source != h.Source || saved.Tool != h.Tool || saved.Arguments != h.Arguments {
			return nil, invalid("continuation does not match this call")
		}
		h.Uploaded = saved.Uploaded
		if saved.Value != "" {
			body["requestState"] = saved.Value
		}
	}
	peer, supportsFiles := r.session.InitializeResult().Capabilities.Extensions[filesExt]
	var pending map[string]fileInfo
	if supportsFiles && settings(peer)["revision"] == nil {
		b.mu.RLock()
		opts := b.fileCapabilities(p, scope)
		b.mu.RUnlock()
		var err error
		pending, err = fileInputs(r.tools[h.Tool].InputSchema, canonical, min(inlineLimit(settings(peer)), inlineLimit(opts)))
		if err != nil {
			return toolFailure(err.Error()), nil
		}
		if len(pending) > 0 {
			if settings(peer)["upload"] == nil {
				return toolFailure("upstream cannot accept out-of-band files"), nil
			}
			if fileSettings(req.Params.Meta)["upload"] == nil {
				return nil, missingCapability(filesExt, Object{"upload": Object{}})
			}
		}
	}
	if req.Params.InputResponses != nil {
		body["inputResponses"] = req.Params.InputResponses
	}
	out, err := forward(ctx, r, "tools/call", body, req.Params.Meta)
	if err != nil {
		var rpc *jsonrpc.Error
		if h.Uploaded && !errors.As(err, &rpc) {
			return toolFailure("Upload retry outcome is unknown; the tool may have run"), nil
		}
		return nil, err
	}
	switch out["resultType"] {
	case "task":
		if !extension(capabilities(req.Params.Meta), tasksExt) {
			return nil, missingCapability(tasksExt, Object{})
		}
		id, ok := out["taskId"].(string)
		if !ok || id == "" {
			return nil, errors.New("upstream task has no ID")
		}
		h.Value = id
		out["taskId"], err = b.encodeRoute(p, "task", h)
	case "input_required", "upload_required":
		if out["resultType"] == "upload_required" && h.Uploaded {
			return toolFailure("Upstream requested a second upload for the same call"), nil
		}
		if out["resultType"] == "upload_required" {
			h.Uploaded = true
		}
		if out["resultType"] == "upload_required" && fileSettings(req.Params.Meta)["upload"] == nil {
			return nil, missingCapability(filesExt, Object{"upload": Object{}})
		}
		state, _ := out["requestState"].(string)
		if out["resultType"] == "upload_required" && state == "" {
			return nil, invalid("upstream omitted upload requestState")
		}
		h.Value = state
		h.Expires = time.Now().Add(2 * time.Hour).Unix()
		out["requestState"], err = b.encodeRoute(p, "continuation", h)
	}
	if err != nil {
		return nil, err
	}
	if err = b.fileTargets(ctx, p, h, out, pending); err != nil {
		return toolFailure(err.Error()), nil
	}
	return relayed(out), nil
}

func (b *Bridge) taskRequest(ctx context.Context, p Principal, scope, method string, in *taskParams) (*extResult, error) {
	if in.Meta[mcp.MetaKeyProtocolVersion] != modernVersion || !extension(capabilities(in.Meta), tasksExt) {
		return nil, missingCapability(tasksExt, Object{})
	}
	h, err := b.decodeRoute(p, "task", in.Task)
	if err != nil {
		return nil, err
	}
	if scope != "" && scope != h.Source {
		return nil, invalid("task unavailable")
	}
	if method == "tasks/update" {
		var responses map[string]json.RawMessage
		if json.Unmarshal(in.Responses, &responses) != nil || responses == nil {
			return nil, invalid("expected inputResponses object")
		}
	}
	if h.Source == "" {
		return b.localTask(ctx, p, method, h, in)
	}
	r, now, err := b.relayTarget(p, h.Source, h.Tool)
	if err != nil || now.Origin != h.Origin {
		return nil, invalid("task unavailable")
	}
	body := Object{"taskId": h.Value}
	if in.Responses != nil {
		body["inputResponses"] = in.Responses
	}
	out, err := forward(ctx, r, method, body, in.Meta)
	if err != nil {
		return nil, err
	}
	if id, ok := out["taskId"]; ok {
		if id != h.Value {
			return nil, errors.New("unexpected upstream task ID")
		}
		out["taskId"] = in.Task
	}
	if res, ok := out["result"].(map[string]any); ok {
		if err = b.fileTargets(ctx, p, h, res, nil); err != nil {
			return nil, err
		}
	}
	return relayed(out), nil
}
func (b *Bridge) relayMethods(v *view) {
	for _, method := range []string{"tasks/get", "tasks/update", "tasks/cancel"} {
		err := mcp.AddReceivingCustomMethod(v.server, method, func(ctx context.Context, _ *mcp.ServerSession, in *taskParams) (*extResult, error) {
			return b.taskRequest(ctx, v.user, v.scope, method, in)
		})
		if err != nil {
			panic(err)
		}
	}
	v.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if list, ok := request.(*mcp.ListToolsRequest); ok && v.scope == "" && method == "tools/list" && (list.Params == nil || fileSettings(list.Params.Meta) == nil) {
				res, err := next(ctx, method, request)
				if err != nil {
					return nil, err
				}
				out := *res.(*mcp.ListToolsResult)
				visible := make([]*mcp.Tool, 0, len(out.Tools))
				b.mu.RLock()
				for _, t := range out.Tools {
					if _, alias := v.fileTools[t.Name]; !alias || !(strings.HasPrefix(t.Name, "file_") && generatedToolAlias.MatchString(t.Name)) {
						visible = append(visible, t)
					}
				}
				b.mu.RUnlock()
				out.Tools = visible
				return &out, nil
			}
			if method == "server/discover" {
				res, err := next(ctx, method, request)
				if err != nil {
					return nil, err
				}
				d := *res.(*mcp.DiscoverResult)
				caps := *d.Capabilities
				caps.Extensions = maps.Clone(caps.Extensions)
				delete(caps.Extensions, filesExt)
				b.mu.RLock()
				if options := b.fileCapabilities(v.user, v.scope); options != nil {
					caps.Extensions[filesExt] = options
				}
				b.mu.RUnlock()
				d.Capabilities = &caps
				return &d, nil
			}
			req, ok := request.(*mcp.CallToolRequest)
			if !ok || method != "tools/call" || req.Params.Meta[mcp.MetaKeyProtocolVersion] != modernVersion {
				return next(ctx, method, request)
			}
			if v.scope == "" && req.Params.Name == "run_trigger_now" && extension(capabilities(req.Params.Meta), tasksExt) {
				return b.startLocalTask(ctx, v.user, req, next)
			}
			var id string
			args := req.Params.Arguments
			if v.scope == "" && req.Params.Name == "call_tool" {
				var wrapper struct {
					ID    string          `json:"tool_id"`
					Args  json.RawMessage `json:"arguments"`
					Scope string          `json:"task_scope"`
				}
				if json.Unmarshal(args, &wrapper) != nil {
					return nil, invalid("invalid call_tool arguments")
				}
				id, args = wrapper.ID, wrapper.Args
				var err error
				ctx, err = scopedContext(ctx, wrapper.Scope, req.Params.Meta)
				if err != nil {
					return nil, err
				}
			} else {
				b.mu.RLock()
				id = v.fileTools[req.Params.Name]
				b.mu.RUnlock()
				if id == "" {
					return next(ctx, method, request)
				}
			}
			src, name, _ := strings.Cut(id, "/")
			if src == "bridge.builtin" {
				return next(ctx, method, request)
			}
			r, h, err := b.relayTarget(v.user, src, name)
			if err != nil {
				return nil, err
			}
			if r.session.InitializeResult().ProtocolVersion != modernVersion {
				return next(ctx, method, request)
			}
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			result, err := b.relayTool(ctx, v.user, v.scope, r, h, args, req)
			if err == nil {
				out := result.(*extResult).Fields
				if scope, _ := ctx.Value(taskReadScopeKey{}).(string); scope != "" && out["resultType"] == "complete" && out["isError"] != true && r.tools[name].Annotations != nil && r.tools[name].Annotations.ReadOnlyHint {
					var values Object
					if json.Unmarshal(args, &values) == nil {
						_ = b.recordTaskRead(ctx, v.user.Name, scope, id, values)
					}
				}
			}
			return result, err
		}
	})
}

func relayed(out Object) *extResult {
	meta, _ := out["_meta"].(map[string]any)
	if meta == nil {
		meta = Object{}
	}
	delete(meta, mcp.MetaKeyServerInfo)
	return &extResult{ResultBase: mcp.ResultBase{Meta: meta}, Fields: out}
}
func toolFailure(message string) *extResult {
	return complete(Object{"isError": true, "content": []Object{{"type": "text", "text": message}}})
}
func settings(value any) Object {
	raw, _ := json.Marshal(value)
	var out Object
	_ = json.Unmarshal(raw, &out)
	return out
}
func fileSettings(meta mcp.Meta) Object {
	exts, _ := capabilities(meta)["extensions"].(map[string]any)
	opts, _ := exts[filesExt].(map[string]any)
	if opts["revision"] != nil {
		return nil
	}
	return opts
}

// Normalize decimal spellings without losing precision or expanding large exponents.
func normalizedNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		obj := Object{}
		for key, val := range x {
			obj[key] = normalizedNumbers(val)
		}
		return obj
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalizedNumbers(val)
		}
		return out
	case json.Number:
		mantissa, exp, _ := strings.Cut(strings.ToLower(string(x)), "e")
		var power big.Int
		if exp != "" {
			power.SetString(exp, 10)
		}
		sign := ""
		if strings.HasPrefix(mantissa, "-") {
			sign = "-"
			mantissa = mantissa[1:]
		}
		whole, frac, _ := strings.Cut(mantissa, ".")
		digits := strings.TrimLeft(whole+frac, "0")
		if digits == "" {
			return json.Number("0")
		}
		short := strings.TrimRight(digits, "0")
		power.Add(&power, big.NewInt(int64(len(digits)-len(short)-len(frac))))
		return json.Number(sign + short + "e" + power.String())
	}
	return v
}

// Called under b.mu. Advertise the common inline limit across visible file sources.
func (b *Bridge) fileCapabilities(p Principal, scope string) Object {
	var result Object
	var limit int64 = inlineBudget
	for id, r := range b.live {
		if s := b.sources[id]; s.Disabled || !allowed(p, s) || scope != "" && id != scope {
			continue
		}
		raw, ok := r.session.InitializeResult().Capabilities.Extensions[filesExt]
		if !ok {
			continue
		}
		opts := settings(raw)
		if opts["revision"] != nil {
			continue
		}
		if result == nil {
			result = Object{}
		}
		if opts["upload"] != nil {
			result["upload"] = Object{}
		}
		limit = min(limit, inlineLimit(opts))
	}
	if result != nil && limit > 0 {
		result["maxInlineBytes"] = float64(limit)
	}
	return result
}

// Poll at the source's requested interval. The subscription lives only as long
// as the caller's stream. Every fetch uses the same access checks as tasks/get.
func (b *Bridge) taskStream(ctx context.Context, p Principal, r streamRequest, send func(Object) error) error {
	if r.ID == nil || r.Params.Meta[mcp.MetaKeyProtocolVersion] != modernVersion {
		return invalid("invalid subscription")
	}
	if !extension(capabilities(r.Params.Meta), tasksExt) {
		return missingCapability(tasksExt, Object{})
	}
	type watch struct {
		snapshot Object
		last     string
		next     time.Time
	}
	watches := map[string]*watch{}
	ids := []string{}
	get := func(id string) (Object, error) {
		res, err := b.taskRequest(ctx, p, "", "tasks/get", &taskParams{ParamsBase: r.Params.ParamsBase, Task: id})
		if err != nil {
			return nil, err
		}
		delete(res.Fields, "resultType")
		delete(res.Fields, "_meta")
		return res.Fields, nil
	}
	for _, id := range r.Params.Notifications.Tasks {
		if watches[id] != nil {
			continue
		}
		state, err := get(id)
		if err != nil {
			return err
		}
		watches[id] = &watch{snapshot: state}
		ids = append(ids, id)
	}
	notify := func(method string, params Object) error {
		params["_meta"] = Object{mcp.MetaKeySubscriptionID: r.ID}
		return send(Object{"jsonrpc": "2.0", "method": method, "params": params})
	}
	if err := notify("notifications/subscriptions/acknowledged", Object{"notifications": Object{"taskIds": ids}}); err != nil {
		return err
	}
	for {
		var next time.Time
		for id, w := range watches {
			if !time.Now().Before(w.next) {
				state := w.snapshot
				w.snapshot = nil
				if state == nil {
					var err error
					state, err = get(id)
					if err != nil {
						return err
					}
				}
				raw, err := json.Marshal(state)
				if err != nil {
					return err
				}
				if signature := digest(raw); signature != w.last {
					w.last = signature
					if err := notify("notifications/tasks", state); err != nil {
						return err
					}
				}
				switch state["status"] {
				case "cancelled", "completed", "failed":
					delete(watches, id)
					continue
				}
				var delay int64 = 1000
				if n, ok := state["pollIntervalMs"].(json.Number); ok {
					if ms, err := n.Int64(); err == nil {
						delay = max(100, min(ms, int64(3600000)))
					}
				}
				w.next = time.Now().Add(time.Duration(delay) * time.Millisecond)
			}
			if next.IsZero() || w.next.Before(next) {
				next = w.next
			}
		}
		if next.IsZero() {
			break
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			break
		}
	}
	return send(Object{"jsonrpc": "2.0", "id": r.ID, "result": Object{"resultType": "complete", "_meta": Object{mcp.MetaKeySubscriptionID: r.ID}}})
}
