package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"

	"strings"

	"newmcp/mcp-bridge/internal/mcpinfo"
	"newmcp/mcp-bridge/internal/plugin"
)

type sourceRevision struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

func (b *Bridge) management(v *view) {
	tool(v, "bridge_get_status", "Discover webhook, terminal and CLI destinations and the current terminal ID. Inspect Bridge health, delivery backlog, published tool revisions or a source configuration. Pass destination_id to read the effective definition and revision for editing.", func(ctx context.Context, in struct {
		Source      string `json:"source_id,omitempty"`
		Destination string `json:"destination_id,omitempty"`
	}) (any, error) {
		if in.Destination != "" {
			if in.Source != "" {
				return nil, errors.New("select one source or destination")
			}
			d, rev, err := b.effectiveDestination(ctx, v.user, in.Destination)
			if err != nil {
				return nil, err
			}
			return Object{"destination": d, "revision": rev}, nil
		}
		if in.Source != "" {
			return b.sourceStatus(in.Source)
		}
		rows, e := b.store.DB.Query("SELECT status,COUNT(*) FROM deliveries GROUP BY status")
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		counts := Object{}
		for rows.Next() {
			var st string
			var n int
			if e = rows.Scan(&st, &n); e != nil {
				return nil, e
			}
			counts[st] = n
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		backlog, err := b.redactedMetrics()
		if err != nil {
			return nil, err
		}
		rows, e = b.store.DB.Query("SELECT ts,actor,action,id FROM audit ORDER BY seq DESC LIMIT 25")
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		changes := []Object{}
		for rows.Next() {
			var ts int64
			var actor, action, id string
			if e = rows.Scan(&ts, &actor, &action, &id); e != nil {
				return nil, e
			}
			changes = append(changes, Object{"at": ts, "actor": actor, "action": action, "id": id})
		}
		if e = rows.Err(); e != nil {
			return nil, e
		}
		b.mu.RLock()
		ready := map[string]bool{}
		for id, source := range b.sources {
			ready[id] = !source.Disabled && (b.live[id] != nil || source.Type == "package" && source.Server == "")
		}
		b.mu.RUnlock()
		rows, e = b.store.DB.Query("SELECT kind,id,revision FROM objects WHERE kind IN ('binding','destination') ORDER BY kind,id")
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		revisions := map[string]map[string]int{"binding": {}, "destination": {}}
		for rows.Next() {
			var kind, id string
			var rev int
			if e := rows.Scan(&kind, &id, &rev); e != nil {
				return nil, e
			}
			revisions[kind][id] = rev
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		destinations, err := b.destinationStatus(v.user, revisions["destination"])
		if err != nil {
			return nil, err
		}
		current, _ := ctx.Value(terminalBinding{}).(string)
		return Object{"destinations": destinations, "current_terminal": current, "published_tools": revisions["binding"], "webhooks": revisions["destination"], "sources": ready, "recent_changes": changes, "deliveries": counts, "backlog": backlog, "version": mcpinfo.Implementation.Version}, err
	})
}

var secretName = regexp.MustCompile("^[A-Z][A-Z0-9_]{0,63}$")

func secretValue(name string) (string, error) {
	if !secretName.MatchString(name) {
		return "", errors.New("invalid credential reference")
	}
	v, ok := os.LookupEnv("BRIDGE_CRED_" + name)
	if !ok {
		return "", fmt.Errorf("credential %s unavailable", name)
	}
	return v, nil
}
func configValue(v any, def plugin.Setting) (string, error) {
	if def.Sensitive {
		ref, ok := v.(map[string]any)
		if !ok {
			return "", errors.New("expected credential reference")
		}
		name, ok := ref["credential"].(string)
		if !ok {
			return "", errors.New("expected credential reference")
		}
		return secretValue(name)
	}
	if def.Multiple {
		if vals, ok := v.([]any); ok {
			parts := []string{}
			for _, x := range vals {
				st, ok := x.(string)
				if !ok {
					return "", errors.New("expected strings")
				}
				parts = append(parts, st)
			}
			return strings.Join(parts, string(os.PathListSeparator)), nil
		}
		return "", errors.New("expected multiple paths")
	}
	switch def.Type {
	case "string", "file", "directory":
		if s, ok := v.(string); ok {
			return s, nil
		}
	case "number":
		if s, ok := v.(float64); ok {
			return fmt.Sprint(s), nil
		}
	case "boolean":
		if s, ok := v.(bool); ok {
			return fmt.Sprint(s), nil
		}
	}
	return "", errors.New("setting type mismatch")
}
func sameConnection(a, b Source) bool {
	return reflect.DeepEqual(a.OAuth, b.OAuth) && a.Type == b.Type && a.Command == b.Command && a.URL == b.URL && a.Cwd == b.Cwd && reflect.DeepEqual(a.Args, b.Args) && reflect.DeepEqual(a.Env, b.Env) && reflect.DeepEqual(a.Headers, b.Headers)
}
