package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type upstreamHook struct {
	Secret, ID string
	RenewAt    time.Time
}

func (b *Bridge) refreshHook(ctx context.Context, p Principal, f Flow) error {
	var sub upstreamHook
	rev, e := b.store.Get("upstream-hook", f.ID, &sub)
	if e != nil {
		key := make([]byte, 32)
		if _, e = rand.Read(key); e != nil {
			return e
		}
		sub.Secret = "whsec_" + base64.StdEncoding.EncodeToString(key)
		if e = b.store.Put(p.Name, "upstream-hook", f.ID, sub, 0); e != nil {
			return e
		}
		rev = 1
	}
	if time.Now().Before(sub.RenewAt) {
		return nil
	}
	base := strings.TrimRight(os.Getenv("BRIDGE_PUBLIC_URL"), "/")
	if !strings.HasPrefix(base, "https://") {
		return errors.New("BRIDGE_PUBLIC_URL must be HTTPS for inbound callbacks")
	}
	session, e := b.session(p, f.Source)
	if e != nil {
		return e
	}
	var cursor any
	_, _ = b.store.Get("cursor", f.ID, &cursor)
	ttl := int64(3600000)
	result, e := external(ctx, session, eventsExt, "events/subscribe", &extParams{Name: f.Name, Arguments: f.Filter, Cursor: cursor, TTL: &ttl, Delivery: &callback{"webhook", base + "/callback/" + f.ID, sub.Secret}})
	if e != nil {
		return e
	}
	sub.ID, _ = result["id"].(string)
	if sub.ID == "" {
		return errors.New("missing upstream subscription id")
	}
	expire, ok := result["refreshBefore"].(string)
	if ok {
		t, e := time.Parse(time.RFC3339Nano, expire)
		if e != nil {
			return e
		}
		sub.RenewAt = time.Now().Add(time.Until(t) * 2 / 3)
	} else {
		sub.RenewAt = time.Now().Add(time.Hour)
	}
	return b.store.Put(p.Name, "upstream-hook", f.ID, sub, rev)
}
func (b *Bridge) receiveHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("flow")
	var f Flow
	var hook upstreamHook
	if _, e := b.store.Get("flow", id, &f); e != nil || f.Disabled || f.Mode != "webhook" || !b.visible(Principal{f.Owner}, f.Source) {
		http.NotFound(w, r)
		return
	}
	if _, e := b.store.Get("upstream-hook", id, &hook); e != nil {
		http.NotFound(w, r)
		return
	}
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	stamp := r.Header.Get("webhook-timestamp")
	sec, err := strconv.ParseInt(stamp, 10, 64)
	key, kerr := signingKey(hook.Secret)
	if e != nil || err != nil || kerr != nil || sec < time.Now().Unix()-300 || sec > time.Now().Unix()+300 {
		http.Error(w, "invalid callback", 400)
		return
	}
	token := signature(key, r.Header.Get("webhook-id"), stamp, raw)
	valid := false
	for _, s := range strings.Fields(r.Header.Get("webhook-signature")) {
		if hmac.Equal([]byte(s), []byte(token)) {
			valid = true
		}
	}
	if !valid {
		http.Error(w, "invalid signature", 401)
		return
	}
	var control struct {
		Type, Challenge string
		Cursor          any
	}
	if e = json.Unmarshal(raw, &control); e != nil {
		http.Error(w, "invalid callback", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if control.Type == "verification" {
		json.NewEncoder(w).Encode(Object{"challenge": control.Challenge})
		return
	}
	if hook.ID == "" {
		http.Error(w, "subscription pending", 503)
		return
	}
	if r.Header.Get("X-MCP-Subscription-Id") != hook.ID {
		http.Error(w, "subscription mismatch", 403)
		return
	}
	switch control.Type {
	case "gap":
		e = b.appendObserved(r.Context(), f, nil, control.Cursor)
	case "terminated":
		instance := f.Instance
		rev, err := b.store.Get("flow", id, &f)
		if err != nil {
			e = err
		} else if f.Instance == instance {
			f.Disabled = true
			_, e = b.writeFlow(f, Principal{"system"}, rev)
		}
	case "":
		var ev Occurrence
		e = json.Unmarshal(raw, &ev)
		if e == nil && ev.Name != f.Name {
			e = errors.New("unexpected event")
		}
		if e == nil {
			e = b.appendObserved(r.Context(), f, []Occurrence{ev}, ev.Cursor)
		}
	default:
		e = errors.New("unknown control")
	}
	if e != nil {
		http.Error(w, "callback failed", 400)
		return
	}
	w.WriteHeader(204)
}
func (b *Bridge) streamFlow(ctx context.Context, p Principal, f Flow) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(30*time.Second, cancel)
	defer idle.Stop()
	interval := 30 * time.Second
	var cur any
	_, _ = b.store.Get("cursor", f.ID, &cur)
	var config Source
	if _, e := b.store.Get("source", f.Source, &config); e != nil {
		return e
	}
	src, e := b.resolve(config)
	if e != nil {
		return e
	}
	// All three carriers feed the same durable occurrence/cursor transaction.
	frame := func(data []byte) error {
		var r struct {
			Method string
			Params json.RawMessage
			Error  json.RawMessage
		}
		if e := json.Unmarshal(data, &r); e != nil {
			return e
		}
		if len(r.Error) > 0 {
			return errors.New("upstream stream failed")
		}
		if r.Method == "" {
			return nil
		}
		var params struct {
			Cursor    any
			Truncated bool
			Heartbeat int64 `json:"heartbeatMs"`
		}
		if e := json.Unmarshal(r.Params, &params); e != nil {
			return e
		}
		if !b.visible(p, f.Source) {
			return errors.New("source access revoked")
		}
		if r.Method == "notifications/events/active" && params.Heartbeat > 0 {
			interval = time.Duration(min(params.Heartbeat, 3600000)) * time.Millisecond * 3
		}
		idle.Reset(interval)
		if params.Truncated {
			if _, err := b.store.DB.ExecContext(ctx, "INSERT INTO audit(ts,actor,action,id) VALUES(unixepoch(),'system','cursor_gap',?)", f.ID); err != nil {
				return err
			}
		}
		switch r.Method {
		case "notifications/events/active", "notifications/events/heartbeat":
			return b.appendObserved(ctx, f, nil, params.Cursor)
		case "notifications/events/event":
			var ev Occurrence
			if e := json.Unmarshal(r.Params, &ev); e != nil {
				return e
			}
			if ev.Name != f.Name {
				return errors.New("unexpected event")
			}
			return b.appendObserved(ctx, f, []Occurrence{ev}, ev.Cursor)
		case "notifications/events/terminated":
			return errors.New("upstream subscription terminated")
		}
		return nil
	}
	b.mu.RLock()
	r := b.live[f.Source]
	b.mu.RUnlock()
	if r == nil || r.hub == nil {
		return errors.New("source unavailable")
	}
	if src.Type == "streamable-http" {
		return modernRPC(ctx, src, r.oauth, "events/stream", f.Name, Object{"name": f.Name, "arguments": f.Filter, "cursor": cur}, frame)
	}
	return r.hub.stream(ctx, r.session, f, cur, frame)
}
func (b *Bridge) maintainPush(f Flow, p Principal) {
	ctx, end := context.WithCancel(b.ctx)
	if _, exists := b.activeStreams.LoadOrStore(f.ID, end); exists {
		end()
		return
	}
	b.streamWG.Go(func() { defer b.activeStreams.Delete(f.ID); defer end(); _ = b.streamFlow(ctx, p, f) })
}
