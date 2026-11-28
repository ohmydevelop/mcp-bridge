package bridge

import (
	"context"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type flowInput struct {
	ID          string           `json:"trigger_id"`
	Revision    int              `json:"revision,omitempty"`
	When        *triggerWhen     `json:"when,omitempty"`
	Handler     *toolVersion     `json:"handler,omitempty"`
	Replay      *bool            `json:"replay,omitempty"`
	Promotion   *promotionPolicy `json:"promotion,omitempty"`
	Destination string           `json:"destination_id,omitempty" jsonschema:"Destination ID from bridge_get_status, or current_terminal for this bound stdio connection."`
	Instruction *string          `json:"instruction,omitempty" jsonschema:"Task for the receiving agent. Omit to preserve or send an empty string to clear."`
	Skill       *string          `json:"skill_ref,omitempty"`
	Disabled    *bool            `json:"disabled,omitempty"`
}

func (b *Bridge) writeFlow(f Flow, p Principal, rev int) (Object, error) {
	tx, e := b.store.DB.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if e = b.store.PutTx(tx, p.Name, "flow", f.ID, f, rev); e != nil {
		return nil, e
	}
	if f.Disabled {
		if _, e = tx.Exec("UPDATE trigger_runs SET status='cancelled' WHERE instance=? AND status='pending'", f.Instance); e != nil {
			return nil, e
		}
		if _, e = tx.Exec("UPDATE deliveries SET status='suppressed' WHERE flow=? AND status='pending'", f.ID); e != nil {
			return nil, e
		}
	}
	if f.Schedule != nil && (rev == 0 || f.resetClock || f.Disabled) {
		if _, e = tx.Exec("INSERT OR REPLACE INTO trigger_clock VALUES(?,unixepoch())", f.Instance); e != nil {
			return nil, e
		}
	}
	var saved int
	if e = tx.QueryRow("SELECT revision FROM objects WHERE kind='flow' AND id=?", f.ID).Scan(&saved); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	if f.Disabled {
		if stop, ok := b.activeStreams.Load(f.ID); ok {
			stop.(context.CancelFunc)()
		}
	}
	return Object{"trigger_id": f.ID, "destination_id": f.Destination, "disabled": f.Disabled, "revision": saved}, nil
}
func (b *Bridge) producerTool(s *mcp.Server, source string) {
	tool(&view{server: s}, "publish_event", "Report an occurrence from this credential's bound event source.", func(ctx context.Context, in struct {
		ID   string `json:"event_id"`
		Name string `json:"event_name"`
		Data Object `json:"data"`
	}) (any, error) {
		b.mu.RLock()
		src := b.sources[source]
		_, ok := src.Events[in.Name]
		b.mu.RUnlock()
		if !ok || src.Disabled {
			return nil, errors.New("event not declared by active source")
		}
		return Object{"accepted": true}, b.appendEvents(ctx, source, []Occurrence{{ID: in.ID, Name: in.Name, Data: in.Data}}, "", nil)
	})
}
