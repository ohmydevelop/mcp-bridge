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
	tool(v.server, "bridge_delete_destination", "Delete a webhook destination by ID and current revision. It must have no dependent flows.", func(_ context.Context, in sourceRevision) (any, error) {
		if in.Revision < 1 {
			return nil, errors.New("existing revision required")
		}
		b.modify.Lock()
		defer b.modify.Unlock()
		flows, e := storage.List[Flow](b.store, "flow")
		if e != nil {
			return nil, e
		}
		for _, f := range flows {
			if f.Destination == in.ID {
				return nil, fmt.Errorf("destination referenced by %s", f.ID)
			}
		}
		return Object{"removed": in.ID}, b.store.Put(p.Name, "destination", in.ID, nil, in.Revision)
	})

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
