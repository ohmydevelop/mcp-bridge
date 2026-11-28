package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (b *Bridge) startLocalTask(ctx context.Context, p Principal, req *mcp.CallToolRequest, next mcp.MethodHandler) (mcp.Result, error) {
	var in manualRunInput
	dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.ID == "" {
		return nil, invalid("invalid run arguments")
	}
	if in.Dry {
		return next(ctx, "tools/call", req)
	}
	run, err := b.runNow(ctx, p, in)
	if err != nil {
		return nil, err
	}
	h := routeHandle{Tool: in.ID, Value: run["run_id"].(string)}
	token, err := b.encodeRoute(p, "task", h)
	if err != nil {
		return nil, err
	}
	out, err := b.localTask(ctx, p, "tasks/get", h, &taskParams{Task: token})
	if err == nil {
		out.Fields["resultType"] = "task"
	}
	return out, err
}
func (b *Bridge) localTask(ctx context.Context, p Principal, method string, h routeHandle, in *taskParams) (*extResult, error) {
	var f Flow
	var created, updated int64
	var retired sql.NullInt64
	if err := b.store.DB.QueryRowContext(ctx, `SELECT trigger_id,instance,owner,created,coalesce((SELECT updated FROM run_changes WHERE id=r.id),created),(SELECT retired_at FROM retired_instances WHERE instance=r.instance)
		FROM trigger_runs r WHERE id=? AND trigger_id=? AND owner=?`, h.Value, h.Tool, p.Name).Scan(&f.ID, &f.Instance, &f.Owner, &created, &updated, &retired); err != nil {
		return nil, invalid("task unavailable")
	}
	switch method {
	case "tasks/update":
		return complete(Object{}), nil
	case "tasks/cancel":
		tx, err := b.store.DB.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		r, err := tx.Exec("UPDATE trigger_runs SET status='cancelled' WHERE id=? AND status='pending'", h.Value)
		if err != nil {
			return nil, err
		}
		count, err := r.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count == 0 {
			_, err = tx.Exec("UPDATE deliveries SET status='suppressed' WHERE id=? AND status='pending' AND EXISTS(SELECT 1 FROM local_deliveries WHERE id=?)", h.Value, h.Value)
			if err != nil {
				return nil, err
			}

		}
		// Cancellation is cooperative once a delivery has begun.
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return complete(Object{}), nil
	}
	receipt, err := b.runSummary(ctx, f, h.Value)
	if err != nil {
		return nil, err
	}
	out := Object{"resultType": "complete", "taskId": in.Task, "createdAt": time.Unix(created, 0).UTC().Format(time.RFC3339), "lastUpdatedAt": time.Unix(updated, 0).UTC().Format(time.RFC3339), "ttlMs": nil, "pollIntervalMs": 1000}
	if retired.Valid {
		out["ttlMs"] = (retired.Int64 + retiredRetentionSeconds - created) * 1000
	}
	switch receipt["status"] {
	case "pending", "submitting":
		out["status"] = "working"
	case "cancelled", "suppressed":
		out["status"] = "cancelled"
	case "unknown", "too_long", "failed":
		out["status"] = "failed"
		out["error"] = Object{"code": -32603, "message": "Trigger execution or delivery failed", "data": receipt}
	default:
		out["status"] = "completed"
		raw, _ := json.Marshal(receipt)
		out["result"] = Object{"resultType": "complete", "structuredContent": receipt, "content": []Object{{"type": "text", "text": string(raw)}}}
	}
	return &extResult{Fields: out}, nil
}
