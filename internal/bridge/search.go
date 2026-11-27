package bridge

import (
	"context"
	"newmcp/mcp-bridge/internal/storage"
	"sort"
	"strings"
)

type searchHit struct {
	id, name, description string
	aliases               []string
	item                  Object
}

func (h searchHit) rank(query string) int {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" || q == strings.ToLower(h.id) {
		return 0
	}
	terms := append([]string{h.name}, h.aliases...)
	rank := 4
	for _, term := range terms {
		s := strings.ToLower(strings.TrimSpace(term))
		if s == "" {
			continue
		}
		if s == q {
			return 1
		}
		if strings.Contains(s, q) || strings.Contains(q, s) {
			rank = 2
		}
	}
	if rank == 4 && strings.Contains(strings.ToLower(h.description), q) {
		return 3
	}
	return rank
}
func searchPage(key string, hits []searchHit, q inputQuery) Object {
	type match struct {
		hit  searchHit
		rank int
	}
	var matches []match
	for _, h := range hits {
		if r := h.rank(q.Query); r < 4 {
			matches = append(matches, match{h, r})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		return matches[i].hit.id < matches[j].hit.id
	})
	start := min(max(0, q.Offset), len(matches))
	end := min(start+5, len(matches))
	items := []Object{}
	for _, m := range matches[start:end] {
		items = append(items, m.hit.item)
	}
	next := end
	if next == len(matches) {
		next = 0
	}
	return Object{key: items, "next_offset": next}
}
func (b *Bridge) aliases(source, key string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.sources[source].Aliases[key]
}
func (b *Bridge) searchTools(p Principal, q inputQuery) (Object, error) {
	defs, e := storage.List[Binding](b.store, "binding")
	if e != nil {
		return nil, e
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	var hits []searchHit
	add := func(source, name, desc string, aliases []string, schema any) {
		id := source + "/" + name
		hits = append(hits, searchHit{id, name, desc, aliases, Object{"tool_id": id, "description": desc, "inputSchema": schema}})
	}
	for id, r := range b.live {
		s := b.sources[id]
		if !allowed(p, s) || s.Disabled || q.Source != "" && q.Source != id {
			continue
		}
		for n, t := range r.tools {
			if !member(s.Hidden, n) {
				add(id, n, t.Description, s.Aliases["tool:"+n], t.InputSchema)
			}
		}
	}
	if q.Source == "" || q.Source == "bridge.builtin" {
		for _, d := range defs {
			if b.bindingAllowed(p, d) {
				add("bridge.builtin", d.ID, d.Description, d.Aliases, d.Schema)
			}
		}
	}
	return searchPage("tools", hits, q), nil
}
func (b *Bridge) searchEvents(ctx context.Context, p Principal, q inputQuery) (Object, error) {
	var ids []string
	b.mu.RLock()
	for id, s := range b.sources {
		if allowed(p, s) && !s.Disabled {
			ids = append(ids, id)
		}
	}
	b.mu.RUnlock()
	ids = append(ids, "bridge.builtin")
	var hits []searchHit
	for _, id := range ids {
		if q.Source != "" && q.Source != id {
			continue
		}
		defs, e := b.eventTypes(ctx, p, id)
		if e != nil {
			return nil, e
		}
		for _, d := range defs {
			hits = append(hits, searchHit{id + "/" + d.Name, d.Name, d.Description, b.aliases(id, "event:"+d.Name), Object{"source_id": id, "event_name": d.Name, "description": d.Description, "filter_schema": d.Input, "delivery": d.Delivery}})
		}
	}
	return searchPage("events", hits, q), nil
}
