package bridge

import (
	"context"
	"database/sql"
)

const retiredRetentionSeconds = 30 * 86400

// Start the grace period at upgrade for archives that predate retirement timestamps.
func migrateRetirement(tx *sql.Tx, active []Flow) error {
	var exists bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='retired_instances')").Scan(&exists); err != nil || exists {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE retired_instances(instance TEXT PRIMARY KEY, retired_at INTEGER NOT NULL);
		CREATE INDEX retirement_due ON retired_instances(retired_at);
		CREATE INDEX IF NOT EXISTS delivery_flow ON deliveries(flow);
		INSERT INTO retired_instances SELECT instance,unixepoch() FROM (
			SELECT instance FROM trigger_runs UNION SELECT instance FROM trigger_reviews
			UNION SELECT substr(rest,1,instr(rest,'/')-1) FROM
				(SELECT substr(flow,10) AS rest FROM deliveries WHERE flow GLOB 'archived/*/*')
		) WHERE instance <> ''`); err != nil {
		return err
	}
	for _, f := range active {
		if _, err := tx.Exec("DELETE FROM retired_instances WHERE instance=?", f.Instance); err != nil {
			return err
		}
	}
	return nil
}

// Commit each retired instance independently so a restart can resume the sweep.
func (b *Bridge) pruneRetired(ctx context.Context) error {
	for {
		tx, err := b.store.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var instance string
		err = tx.QueryRowContext(ctx, `SELECT instance FROM retired_instances
			WHERE retired_at<=unixepoch()-? AND NOT EXISTS (
				SELECT 1 FROM deliveries d WHERE d.flow GLOB ('archived/'||instance||'/*')
				AND d.status IN ('pending','submitting'))
			ORDER BY retired_at LIMIT 1`, retiredRetentionSeconds).Scan(&instance)
		if err != nil {
			tx.Rollback()
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		archive := "archived/" + instance + "/*"
		for _, q := range []struct{ sql, arg string }{
			{"DELETE FROM local_deliveries WHERE id IN (SELECT id FROM deliveries WHERE flow GLOB ?)", archive},
			{"DELETE FROM deliveries WHERE flow GLOB ?", archive},
			{"DELETE FROM trigger_runs WHERE instance=?", instance},
			{"DELETE FROM trigger_reviews WHERE instance=?", instance},
			{"DELETE FROM retired_instances WHERE instance=?", instance},
		} {
			if _, err = tx.ExecContext(ctx, q.sql, q.arg); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
}
