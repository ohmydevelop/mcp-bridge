package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"time"

	"newmcp/mcp-bridge/internal/storage"
)

func (b *Bridge) lifecycleTools(v *view) {
	p := v.user
	tool(v, "bridge_delete_destination", "Delete a webhook or locally managed CLI destination by ID and current revision. It must have no dependent flows.", func(ctx context.Context, in sourceRevision) (any, error) {
		if in.Revision < 1 {
			return nil, errors.New("existing revision required")
		}
		b.modify.Lock()
		defer b.modify.Unlock()
		tx, err := b.store.DB.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		state, err := b.removeUnusedDestination(ctx, tx, p, in.ID, in.Revision)
		if err != nil {
			return nil, err
		}
		if state != "removed" {
			return nil, fmt.Errorf("destination retained: %s", state)
		}
		return Object{"removed": in.ID}, tx.Commit()
	})

}

// expected == 0 requests conditional cleanup as part of deleting the owner's trigger.
func (b *Bridge) removeUnusedDestination(ctx context.Context, tx *sql.Tx, p Principal, id string, expected int) (string, error) {
	var d Destination
	var raw []byte
	var rev int
	err := tx.QueryRowContext(ctx, "SELECT data,revision FROM objects WHERE kind='destination' AND id=?", id).Scan(&raw, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "already_absent", nil
	}
	if err != nil {
		return "", err
	}
	if err = b.store.Decode("destination", id, raw, &d); err != nil {
		return "", err
	}
	if expected > 0 && expected != rev {
		return "", errors.New("revision conflict; reload configuration")
	}
	if expected == 0 && !canUse(p.Name, d.Owner, nil) {
		return "owned_by_another_identity", nil
	}
	if _, file := b.presets[id]; file || (d.kind() != "webhook" && d.kind() != "cli_process") {
		return "host_managed", nil
	}
	if d.kind() == "cli_process" && ctx.Value(localConnectionKey{}) != true {
		return "local_stdio_required", nil
	}
	flows, err := storage.ListTx[Flow](b.store, tx, "flow")
	if err != nil {
		return "", err
	}
	for _, f := range flows {
		if f.Destination == id {
			return "referenced_by_trigger", nil
		}
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM deliveries WHERE destination=? AND status IN ('pending','submitting'))", id).Scan(&busy); err != nil {
		return "", err
	}
	if busy {
		return "delivery_in_progress", nil
	}
	// A queued handler retains its original destination even if its trigger was edited.
	rows, err := tx.QueryContext(ctx, "SELECT id,input FROM trigger_runs WHERE status='pending'")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var run string
		var data []byte
		var in invocation
		if err = rows.Scan(&run, &data); err != nil {
			return "", err
		}
		if err = b.store.Decode("run", run, data, &in); err != nil {
			return "", err
		}
		if in.Destination == id {
			return "run_in_progress", nil
		}
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	return "removed", b.store.PutTx(tx, p.Name, "destination", id, nil, rev)
}
func (b *Bridge) redactedMetrics() (Object, error) {
	var pending, failed int
	var oldest sql.NullInt64
	e := b.store.DB.QueryRow("SELECT COUNT(*) FILTER(WHERE status='pending'), COUNT(*) FILTER(WHERE status='failed'), MIN(next) FILTER(WHERE status='pending') FROM deliveries").Scan(&pending, &failed, &oldest)
	if e != nil {
		return nil, e
	}
	var lag int64
	if oldest.Valid && oldest.Int64 > 0 {
		lag = max(0, time.Now().Unix()-oldest.Int64)
	}
	return Object{"pending": pending, "failed": failed, "overdue_seconds": lag}, nil
}
