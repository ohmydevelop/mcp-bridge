package bridge

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

type manualRunInput struct {
	ID      string `json:"trigger_id"`
	Event   string `json:"event_id,omitempty"`
	Request string `json:"request_id,omitempty"`
	Dry     bool   `json:"dry_run,omitempty"`
}

func (b *Bridge) runNow(ctx context.Context, p Principal, in manualRunInput) (Object, error) {
	b.modify.Lock()
	defer b.modify.Unlock()
	var f Flow
	rev, err := b.store.Get("flow", in.ID, &f)
	if err != nil || f.Owner != p.Name {
		return nil, errors.New("trigger unavailable")
	}
	if f.Disabled {
		return nil, errors.New("trigger is paused")
	}
	var dest Destination
	if _, err = b.store.Get("destination", f.Destination, &dest); err != nil || !canUse(p.Name, dest.Owner, dest.Grants) {
		return nil, errors.New("destination unavailable")
	}
	if f.Handler != nil {
		def, _, err := b.readVersion(*f.Handler, p)
		if err != nil {
			return nil, err
		}
		if err = b.checkBackgroundTools(p, def); err != nil {
			return nil, err
		}
	}
	args := Object{}
	if f.Schedule != nil {
		if in.Event != "" {
			return nil, errors.New("event_id applies to event triggers")
		}
		args["scheduled_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	} else {
		if in.Event == "" {
			return nil, errors.New("event_id required for an event trigger")
		}
		var raw []byte
		if !b.visible(p, f.Source) {
			return nil, errors.New("event unavailable")
		}
		err = b.store.DB.QueryRowContext(ctx, "SELECT data FROM events WHERE source=? AND name=? AND eventid=?", f.Source, f.Name, in.Event).Scan(&raw)
		if err != nil {
			return nil, errors.New("event unavailable")
		}
		var event Occurrence
		if err = b.store.Decode("event", f.Source+"/"+in.Event, raw, &event); err != nil {
			return nil, err
		}
		if !eventVisibleTo(f.Source, event, p.Name) || !matches(f.Filter, event.Data) {
			return nil, errors.New("event unavailable or does not match trigger")
		}
		args["source_id"], args["event"] = f.Source, event
	}
	if in.Dry {
		return Object{"status": "preview", "trigger": triggerView(f, rev), "input": args}, nil
	}
	key := in.Request
	if key == "" {
		key = uuid.NewString()
	}
	id := digest([]byte("manual\x00" + f.Instance + "\x00" + key))
	tx, err := b.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = b.enqueueRun(tx, f, f.Instance, id, args); err != nil {
		return nil, err
	}
	return Object{"trigger_id": f.ID, "run_id": id, "request_id": key}, tx.Commit()
}

func (b *Bridge) latestRun(ctx context.Context, p Principal, trigger, id string, wait int) (Object, error) {
	if wait < 0 || wait > 30000 {
		return nil, errors.New("wait_ms must be between 0 and 30000")
	}
	var f Flow
	if _, err := b.store.Get("flow", trigger, &f); err != nil || f.Owner != p.Name {
		return nil, errors.New("trigger unavailable")
	}
	if id == "" {
		err := b.store.DB.QueryRowContext(ctx, "SELECT id FROM trigger_runs WHERE instance=? AND owner=? ORDER BY rowid DESC LIMIT 1", f.Instance, p.Name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return Object{"trigger_id": trigger, "status": "never_run"}, nil
		}
		if err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Millisecond)
	for wait > 0 && time.Now().Before(deadline) {
		var pending bool
		err := b.store.DB.QueryRowContext(ctx, `SELECT r.status='pending' OR coalesce(d.status IN ('pending','submitting'),0)
			FROM trigger_runs r LEFT JOIN deliveries d ON d.id=r.id
			WHERE r.id=? AND r.instance=? AND r.owner=?`, id, f.Instance, f.Owner).Scan(&pending)
		if err != nil {
			return nil, err
		}
		if !pending {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(150*time.Millisecond, time.Until(deadline))):
		}
	}
	return b.runSummary(ctx, f, id)
}

func (b *Bridge) runSummary(ctx context.Context, f Flow, id string) (Object, error) {
	var created int64
	var status, delivery, kind, deliveryError string
	var input, output, receipt []byte
	var attempts int
	err := b.store.DB.QueryRowContext(ctx, `SELECT r.created,r.status,r.input,CASE WHEN r.status IN ('skipped','failed') THEN r.output END,coalesce(d.status,''),
        CASE WHEN d.id IS NULL THEN '' ELSE coalesce(l.kind,'webhook') END,
        coalesce(d.attempt,0),coalesce(d.error,''),l.result FROM trigger_runs r
        LEFT JOIN deliveries d ON d.id=r.id LEFT JOIN local_deliveries l ON l.id=d.id
        WHERE r.id=? AND r.instance=? AND r.owner=?`, id, f.Instance, f.Owner).Scan(&created, &status, &input, &output, &delivery, &kind, &attempts, &deliveryError, &receipt)
	if err != nil {
		return nil, err
	}
	var in invocation
	if err = b.store.Decode("run", id, input, &in); err != nil {
		return nil, err
	}
	r := Object{"trigger_id": f.ID, "run_id": id, "triggered_at": time.Unix(created, 0).UTC().Format(time.RFC3339), "processing_status": status, "status": status, "destination_id": in.Destination}
	if stamp, ok := in.Args["scheduled_at"]; ok {
		r["scheduled_at"] = stamp
	}
	if in.Handler != nil {
		r["handler"] = in.Handler
	}
	if status == "failed" || status == "skipped" {
		var result struct {
			Error  string `json:"error"`
			Result struct {
				Reason string `json:"reason"`
			} `json:"result"`
		}
		if err = b.store.Decode("out", id, output, &result); err != nil {
			return nil, err
		}
		if status == "failed" {
			r["failure_stage"], r["error"] = "handler", result.Error
		} else {
			r["reason"] = result.Result.Reason
		}
	}
	if delivery != "" {
		r["status"], r["delivery_status"], r["destination_type"], r["attempts"] = delivery, delivery, kind, attempts
		if delivery == "failed" {
			r["failure_stage"] = "delivery"
		}
		if deliveryError != "" {
			r["delivery_message"] = deliveryError
		}
		if len(receipt) > 0 {
			var details Object
			if err = b.store.Decode("delivery-result", id, receipt, &details); err != nil {
				return nil, err
			}
			r["delivery_result"] = details
		}
	}
	return r, nil
}
