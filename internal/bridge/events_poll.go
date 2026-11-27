package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type PollAdapter struct {
	Source string `json:"source_id"`
	Name   string `json:"event_name"`
	Tool   string `json:"tool"`
}

func (b *Bridge) pollAdapter(ctx context.Context, p Principal, a PollAdapter, cur any, args Object) (Object, error) {
	result, e := b.call(ctx, p, a.Source, a.Tool, Object{"cursor": cur, "arguments": args})
	if e != nil {
		return nil, e
	}
	if result.IsError {
		return nil, errors.New("poll adapter failed")
	}
	out, ok := result.StructuredContent.(map[string]any)
	if !ok {
		return nil, errors.New("poll adapter requires a structured batch")
	}
	return out, nil
}
func (b *Bridge) nextPoll(id string, ms float64) {
	b.pollSchedule.Store(id, time.Now().Add(time.Duration(min(max(ms, 1000), 3600000))*time.Millisecond))
}
func (b *Bridge) drainFlow(ctx context.Context, f Flow, p Principal) error {
	if next, ok := b.pollSchedule.Load(f.ID); ok && time.Now().Before(next.(time.Time)) {
		return nil
	}
	s, e := b.session(p, f.Source)
	if e != nil {
		return nil
	}
	b.mu.RLock()
	toolName, local := b.sources[f.Source].Events[f.Name]
	b.mu.RUnlock()
	if local && toolName == "" {
		return nil
	}
	a := PollAdapter{f.Source, f.Name, toolName}
	if !local {
		if _, ok := s.InitializeResult().Capabilities.Extensions[eventsExt]; !ok {
			return nil
		}
	}
	var cur any
	_, _ = b.store.Get("cursor", f.ID, &cur)
	for range 8 {
		var out Object
		if local {
			out, e = b.pollAdapter(ctx, p, a, cur, f.Filter)
		} else {
			out, e = external(ctx, s, eventsExt, "events/poll", &extParams{Name: f.Name, Arguments: f.Filter, Cursor: cur, MaxEvents: 100})
		}
		if e != nil {
			b.nextPoll(f.ID, 10000)
			return e
		}
		var events []Occurrence
		raw, e := json.Marshal(out["events"])
		if e != nil {
			return e
		}
		if e = json.Unmarshal(raw, &events); e != nil {
			return e
		}
		next, ok := out["cursor"]
		if !ok {
			return errors.New("poll result missing cursor")
		}
		for _, ev := range events {
			if ev.Name != f.Name {
				return errors.New("unexpected event in poll result")
			}
		}
		if gap, _ := out["truncated"].(bool); gap {
			_, e = b.store.DB.Exec("INSERT INTO audit(ts,actor,action,id) VALUES(?,?,?,?)", time.Now().Unix(), "system", "cursor_gap", f.ID)
			if e != nil {
				return e
			}
		}
		if e = b.appendObserved(ctx, f, events, next); e != nil {
			return e
		}
		more, _ := out["hasMore"].(bool)
		if !more {
			ms, _ := out["nextPollMs"].(float64)
			b.nextPoll(f.ID, ms)
			return nil
		}
		if equal(cur, next) {
			return errors.New("upstream cursor did not advance")
		}
		cur = next
	}
	return nil
}
