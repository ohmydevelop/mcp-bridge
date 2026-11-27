package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"newmcp/mcp-bridge/internal/storage"
)

type Destination struct {
	Type        string   `json:"type,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Credential  string   `json:"credential,omitempty"`
	Description string   `json:"description,omitempty"`
	Grants      []string `json:"grants,omitempty"`
}
type Flow struct {
	resetClock  bool
	Instance    string           `json:"instance,omitempty"`
	Schedule    *triggerWhen     `json:"schedule,omitempty"`
	Handler     *toolVersion     `json:"handler,omitempty"`
	Replay      bool             `json:"replay,omitempty"`
	Promotion   *promotionPolicy `json:"promotion,omitempty"`
	Mode        string           `json:"mode,omitempty"`
	ID          string           `json:"id"`
	Source      string           `json:"source_id"`
	Name        string           `json:"event_name"`
	Destination string           `json:"destination_id"`
	Filter      Object           `json:"filter,omitempty"`
	Instruction string           `json:"instruction,omitempty"`
	Skill       string           `json:"skill_ref,omitempty"`
	Owner       string           `json:"owner,omitempty"`
	Disabled    bool             `json:"disabled,omitempty"`
}
type Occurrence struct {
	ID        string `json:"eventId"`
	Name      string `json:"name"`
	Timestamp string `json:"timestamp,omitempty"`
	Data      Object `json:"data"`
	Cursor    any    `json:"cursor,omitempty"`
}
type eventType struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Delivery    []string `json:"delivery"`
	Input       Object   `json:"inputSchema"`
	Payload     Object   `json:"payloadSchema"`
}

func eventDecl(name string) eventType {
	return eventType{name, name, []string{"poll", "push", "webhook"}, Object{"type": "object"}, Object{"type": "object"}}
}
func adapterEvent(name string) eventType {
	d := eventDecl(name)
	d.Delivery = []string{"poll"}
	return d
}
func equal(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func matches(filter, data Object) bool {
	for k, v := range filter {
		vv, ok := data[k]
		if !ok || !equal(v, vv) {
			return false
		}
	}
	return true
}
func (b *Bridge) visible(p Principal, id string) bool {
	if id == "bridge.builtin" {
		return true
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.sources[id]
	return ok && !s.Disabled && allowed(p, s)
}
func (b *Bridge) appendEvents(ctx context.Context, src string, events []Occurrence, checkpoint string, cursor any) error {
	tx, e := b.store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = b.appendEventsTx(tx, src, events, checkpoint, cursor); e != nil {
		return e
	}
	return tx.Commit()
}

// A delayed poll or webhook must not advance the cursor of a recreated trigger.
func (b *Bridge) appendObserved(ctx context.Context, f Flow, events []Occurrence, cursor any) error {
	tx, err := b.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	var current Flow
	if err = tx.QueryRow("SELECT data FROM objects WHERE kind='flow' AND id=?", f.ID).Scan(&raw); err != nil {
		return err
	}
	if err = b.store.Decode("flow", f.ID, raw, &current); err != nil {
		return err
	}
	if current.Instance != f.Instance || current.Disabled || !equal(current.Filter, f.Filter) {
		return errors.New("stale event subscription")
	}
	if err = b.appendEventsTx(tx, f.Source, events, f.ID, cursor); err != nil {
		return err
	}
	return tx.Commit()
}
func (b *Bridge) appendEventsTx(tx *sql.Tx, src string, events []Occurrence, checkpoint string, cursor any) error {
	flows, e := storage.ListTx[Flow](b.store, tx, "flow")
	if e != nil {
		return e
	}
	for _, ev := range events {
		if ev.ID == "" || ev.Name == "" || ev.Data == nil {
			return errors.New("event requires eventId, name and data")
		}
		if ev.Timestamp == "" {
			ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
		}
		raw, e := b.store.Seal("event", src+"/"+ev.ID, ev)
		if e != nil {
			return e
		}
		res, e := tx.Exec("INSERT OR IGNORE INTO events(source,eventid,name,data,stamp) VALUES(?,?,?,?,?)", src, ev.ID, ev.Name, raw, ev.Timestamp)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			continue
		}
		for _, f := range flows {
			if !eventVisibleTo(src, ev, f.Owner) {
				continue
			}
			if f.Disabled || f.Source != src || f.Name != ev.Name || !matches(f.Filter, ev.Data) {
				continue
			}
			if e := b.queueTrigger(tx, f, ev.ID, Object{"event": ev, "source_id": src}); e != nil {
				return e
			}
		}
	}
	if checkpoint != "" {
		raw, e := b.store.Seal("cursor", checkpoint, cursor)
		if e != nil {
			return e
		}
		_, e = tx.Exec("INSERT INTO objects VALUES('cursor',?,1,?) ON CONFLICT(kind,id) DO UPDATE SET revision=revision+1,data=excluded.data", checkpoint, raw)
		if e != nil {
			return e
		}
	}
	return nil
}
func (b *Bridge) eventTypes(ctx context.Context, p Principal, source string) ([]eventType, error) {
	if !b.visible(p, source) {
		return nil, errors.New("source unavailable")
	}
	defs := []eventType{}
	if source == "bridge.builtin" {
		for _, name := range builtinEvents {
			defs = append(defs, adapterEvent(name))
		}
		return defs, nil
	}
	b.mu.RLock()
	for n := range b.sources[source].Events {
		defs = append(defs, adapterEvent(n))
	}
	b.mu.RUnlock()
	s, e := b.session(p, source)
	if e == nil {
		if _, ok := s.InitializeResult().Capabilities.Extensions[eventsExt]; ok {
			out, e := external(ctx, s, eventsExt, "events/list", &extParams{})
			if e != nil {
				return nil, e
			}
			var more []eventType
			raw, _ := json.Marshal(out["events"])
			if e = json.Unmarshal(raw, &more); e != nil {
				return nil, e
			}
			defs = append(defs, more...)
		}
	}
	return defs, nil
}
func eventVisibleTo(source string, ev Occurrence, principal string) bool {
	return source != "bridge.builtin" || ev.Name != triggerCompleted || ev.Data["actor"] == principal
}

func (b *Bridge) pollLocal(p Principal, source, name string, args Object, cursor any, limit int) (Object, error) {
	if !b.visible(p, source) {
		return nil, errors.New("source unavailable")
	}
	var seq int64
	if cursor == nil {
		if e := b.store.DB.QueryRow("SELECT COALESCE(MAX(seq),0) FROM events").Scan(&seq); e != nil {
			return nil, e
		}
	} else {
		var e error
		seq, e = strconv.ParseInt(fmt.Sprint(cursor), 10, 64)
		if e != nil || seq < 0 {
			return nil, errors.New("invalid cursor")
		}
	}
	if limit < 1 || limit > 100 {
		limit = 100
	}
	rows, e := b.store.DB.Query("SELECT seq,eventid,data FROM events WHERE source=? AND name=? AND seq>? ORDER BY seq LIMIT ?", source, name, seq, limit+1)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	events := []Occurrence{}
	scanned := 0
	more := false
	for rows.Next() {
		if scanned == limit {
			more = true
			break
		}
		var id string
		var data []byte
		if e = rows.Scan(&seq, &id, &data); e != nil {
			return nil, e
		}
		scanned++
		var ev Occurrence
		if e = b.store.Decode("event", source+"/"+id, data, &ev); e != nil {
			return nil, e
		}
		ev.Cursor = strconv.FormatInt(seq, 10)
		if eventVisibleTo(source, ev, p.Name) && matches(args, ev.Data) {
			events = append(events, ev)
		}
	}
	return Object{"events": events, "cursor": strconv.FormatInt(seq, 10), "hasMore": more, "truncated": false, "nextPollMs": 2000}, rows.Err()
}
func permittedURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, origin := range strings.Split(os.Getenv("BRIDGE_WEBHOOK_ORIGINS"), ",") {
		if origin != "" && origin == u.Scheme+"://"+u.Host && (u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
			return true
		}
	}
	return false
}
func (b *Bridge) eventTools(v *view) {
	b.webhookMethods(v)
	p := v.user
	s := v.server
	tool(s, "search_event_types", "Find events to subscribe to, including the source, filters and delivery modes.", func(ctx context.Context, q inputQuery) (any, error) { return b.searchEvents(ctx, p, q) })
	tool(s, "bridge_save_destination", "Manage a webhook destination. Local terminals and CLI process presets are registered on the host. Use revision 0 to create or the current revision to replace its configuration.", func(_ context.Context, in struct {
		Destination Destination `json:"destination"`
		Revision    int         `json:"revision"`
	}) (any, error) {
		d := in.Destination
		if d.Type == "" {
			d.Type = "webhook"
		}
		if d.Type != "webhook" {
			return nil, errors.New("only webhook destinations are MCP-managed")
		}
		if !identifier.MatchString(d.ID) || !permittedURL(d.URL) {
			return nil, errors.New("invalid id or destination origin")
		}
		if d.Credential != "" {
			if _, e := secretValue(d.Credential); e != nil {
				return nil, e
			}
		}
		b.modify.Lock()
		defer b.modify.Unlock()
		d.Owner = p.Name
		if in.Revision > 0 {
			var previous Destination
			if _, err := b.store.Get("destination", d.ID, &previous); err != nil {
				return nil, err
			}

			d.Owner = previous.Owner
		}
		return Object{"revision": in.Revision + 1}, b.store.Put(p.Name, "destination", d.ID, d, in.Revision)
	})
	tool(s, "save_trigger", "Create an event or five-field cron trigger. Event when: source_id and event_name. Schedule when: five-field cron and IANA timezone. Optional handler takes run(args,ctx) with event or scheduled_at, returns {action:notify,data:...} or {action:skip,reason:...}. Pin its saved tool_id and revision. Omit revision or use 0 to create. Supply the current revision to update.", func(ctx context.Context, in flowInput) (any, error) {
		b.modify.Lock()
		defer b.modify.Unlock()
		if !identifier.MatchString(in.ID) {
			return nil, errors.New("invalid flow_id")
		}
		f := Flow{ID: in.ID, Owner: p.Name}
		have, e := b.store.Get("flow", in.ID, &f)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if have != 0 && f.Owner != p.Name {
			return nil, errors.New("flow unavailable")
		}
		if in.Revision != have {
			return nil, errors.New("revision conflict")
		}
		if e = b.configureTrigger(&f, in, have, p); e != nil {
			return nil, e
		}
		if in.Destination != "" {
			f.Destination = in.Destination

		}
		if in.Instruction != nil {
			f.Instruction = *in.Instruction
		}
		if in.Skill != nil {
			f.Skill = *in.Skill
		}
		if in.Disabled != nil {
			f.resetClock = f.resetClock || f.Disabled != *in.Disabled
			f.Disabled = *in.Disabled
		}
		if f.Disabled {
			if have == 0 {
				return nil, errors.New("cannot disable missing flow")
			}
			return b.writeFlow(f, p, have)
		}
		if f.Destination == "" {
			ds, e := storage.List[Destination](b.store, "destination")
			if e != nil {
				return nil, e
			}
			choices := []Object{}
			for _, d := range ds {
				if canUse(f.Owner, d.Owner, d.Grants) {
					choices = append(choices, Object{"id": d.ID, "description": d.Description, "type": "webhook"})
				}
			}
			if len(choices) != 1 {
				return Object{"status": "destination_required", "destinations": choices}, nil
			}
			f.Destination = choices[0]["id"].(string)
		}
		var d Destination
		if _, e := b.store.Get("destination", f.Destination, &d); e != nil || !canUse(f.Owner, d.Owner, d.Grants) {
			return nil, errors.New("destination unavailable")
		}
		if f.Schedule == nil {
			defs, e := b.eventTypes(ctx, Principal{f.Owner}, f.Source)
			if e != nil {
				return nil, e
			}
			ok := false
			for _, d := range defs {
				if d.Name == f.Name {
					if f.Mode == "" {
						for _, mode := range []string{"poll", "push", "webhook"} {
							if member(d.Delivery, mode) {
								f.Mode = mode
								break
							}
						}
					}
					if !member(d.Delivery, f.Mode) {
						continue
					}
					ok = true
				}
			}
			if !ok {
				return nil, errors.New("event not available for ingestion or polling")
			}
			if f.Mode == "webhook" && !strings.HasPrefix(os.Getenv("BRIDGE_PUBLIC_URL"), "https://") {
				return nil, errors.New("upstream callbacks require BRIDGE_PUBLIC_URL (HTTPS)")
			}
		}
		if f.Skill != "" {
			if _, e = b.findSkill(ctx, Principal{f.Owner}, f.Skill); e != nil {
				return nil, e
			}
		}
		return b.writeFlow(f, p, have)
	})
	b.triggerQueries(v)
	native(v, "events/list", func(ctx context.Context, _ *extParams) (Object, error) {
		defs := []eventType{}
		sources, e := storage.List[Source](b.store, "source")
		if e != nil {
			return nil, e
		}
		sources = append(sources, Source{ID: "bridge.builtin"})
		for _, src := range sources {
			if !b.visible(p, src.ID) {
				continue
			}
			var local []string
			for n := range src.Events {
				local = append(local, n)
			}
			if src.ID == "bridge.builtin" {
				local = builtinEvents
			}
			for _, name := range local {
				defs = append(defs, eventDecl(src.ID+"__"+name))
			}
		}
		return Object{"events": defs, "ttlMs": 0, "cacheScope": "private"}, nil
	})
	native(v, "events/poll", func(_ context.Context, in *extParams) (Object, error) {
		src, name, e := b.localType(p, in.Name)
		if e != nil {
			return nil, e
		}
		state, e := b.pollLocal(p, src, name, in.Arguments, in.Cursor, in.MaxEvents)
		if e == nil {
			events := state["events"].([]Occurrence)
			for i := range events {
				events[i].Name = in.Name
			}
		}
		return state, e
	})
}

// Native subscriptions use an empty flow and their subscription ID as destination.
func (b *Bridge) enqueueDelivery(tx *sql.Tx, id, flow, destination string, body any) error {
	raw, err := b.store.Seal("delivery", id, body)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO deliveries(id,flow,destination,data,status,next) VALUES(?,?,?,?,'pending',unixepoch())", id, flow, destination, raw)

	return err
}

func (b *Bridge) deliver(ctx context.Context) error {
	var id, flow, dest string
	var raw []byte
	var tries int
	e := b.store.DB.QueryRowContext(ctx, "SELECT id,flow,destination,data,attempt FROM deliveries WHERE status='pending' AND next<=? ORDER BY next,rowid LIMIT 1", time.Now().Unix()).Scan(&id, &flow, &dest, &raw, &tries)
	if e != nil {
		return e
	}
	req, e := b.deliveryRequest(ctx, id, flow, dest, raw)
	if e != nil {
		return e
	}
	if req == nil {
		if flow == "" {
			_, e = b.store.DB.Exec("DELETE FROM deliveries WHERE id=? AND data=?", id, raw)
		} else {
			_, e = b.store.DB.Exec("UPDATE deliveries SET status='suppressed' WHERE id=? AND status='pending'", id)
		}
		return e
	}
	response, e := webhookClient.Do(req)
	status := "pending"
	reason := "network"
	if e == nil {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
		response.Body.Close()
		reason = strconv.Itoa(response.StatusCode)
		if response.StatusCode/100 == 2 {
			status = "accepted"
			reason = ""
		} else if response.StatusCode/100 == 4 && response.StatusCode != 429 && response.StatusCode != 408 {
			status = "failed"
		}
	}
	if flow == "" {
		if status == "accepted" {
			_, e = b.store.DB.Exec("DELETE FROM deliveries WHERE id=? AND data=?", id, raw)
			return e
		}
		status = "pending" // Native subscriptions retry until expiry.
	} else if status == "pending" && tries >= 9 {
		status = "failed"
	}
	_, e = b.store.DB.Exec("UPDATE deliveries SET attempt=attempt+1,status=?,error=?,next=? WHERE id=? AND data=? AND status='pending'", status, reason, time.Now().Unix()+int64(1<<min(tries, 9)), id, raw)
	return e
}

var webhookClient = &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectRedirect}

func (b *Bridge) deliveryRequest(ctx context.Context, id, flow, dest string, raw []byte) (*http.Request, error) {
	var data json.RawMessage
	if err := b.store.Decode("delivery", id, raw, &data); err != nil {
		return nil, err
	}
	var e error
	if flow == "" {
		var rec subscription
		if _, e = b.store.Get("subscription", dest, &rec); errors.Is(e, sql.ErrNoRows) {
			return nil, nil
		} else if e != nil {
			return nil, e
		}
		expires, e := time.Parse(time.RFC3339Nano, rec.Expires)
		if e != nil {
			return nil, e
		}
		p := Principal{rec.Owner}
		if !time.Now().Before(expires) || !b.visible(p, rec.Source) || !permittedURL(rec.Delivery.URL) {
			return nil, nil
		}
		return signedRequest(ctx, rec.Delivery, rec.ID, id, data)
	}
	var f Flow
	var d Destination
	if _, e = b.store.Get("flow", flow, &f); e != nil {
		return nil, e
	}
	if _, e = b.store.Get("destination", dest, &d); e != nil {
		return nil, e
	}
	owner := Principal{f.Owner}
	if f.Disabled || (f.Schedule == nil && !b.visible(owner, f.Source)) || !canUse(owner.Name, d.Owner, d.Grants) || !permittedURL(d.URL) {
		return nil, nil
	}
	req, e := http.NewRequestWithContext(ctx, "POST", d.URL, bytes.NewReader(data))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", id)
	if d.Credential != "" {
		secret, err := secretValue(d.Credential)
		if err != nil {
			_, err = b.store.DB.Exec("UPDATE deliveries SET status='failed',error='credential unavailable' WHERE id=?", id)
			return nil, err
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(data)
		req.Header.Set("X-Bridge-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	return req, nil
}

func (b *Bridge) pollUpstreams(ctx context.Context) error {
	flows, e := storage.List[Flow](b.store, "flow")
	if e != nil {
		return e
	}
	for _, f := range flows {
		if f.Disabled {
			continue
		}
		if f.Schedule != nil {
			continue
		}
		owner := Principal{f.Owner}
		pollCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		switch f.Mode {
		case "push":
			b.maintainPush(f, owner)
		case "webhook":
			e = b.refreshHook(pollCtx, owner, f)
		default:
			e = b.drainFlow(pollCtx, f, owner)
		}
		cancel()
		if e != nil && ctx.Err() == nil {
			log.Printf("poll failed for %s", f.ID)
		}

	}
	return nil
}
