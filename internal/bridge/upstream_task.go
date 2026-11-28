package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The worker owns this call. It can wait for a task but has no interactive user
// to answer a form or supply a file. Never fabricate consent in a scheduled run.
func workerCall(ctx context.Context, r *backend, name string, args any) (*mcp.CallToolResult, error) {
	meta := mcp.Meta{mcp.MetaKeyClientCapabilities: Object{"extensions": Object{tasksExt: Object{}}}}
	out, err := forward(ctx, r, "tools/call", Object{"name": name, "arguments": args}, meta)
	if err != nil {
		return nil, err
	}
	if out["resultType"] == "task" {
		id, ok := out["taskId"].(string)
		if !ok || id == "" {
			return nil, errors.New("missing upstream task ID")
		}
		params := Object{"taskId": id}
		cancelTask := func() {
			stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_, _ = forward(stop, r, "tasks/cancel", params, meta)
		}
		for {
			out, err = forward(ctx, r, "tasks/get", params, meta)
			if err != nil {
				if ctx.Err() != nil {
					cancelTask()
				}
				return nil, err
			}
			if out["taskId"] != id {
				return nil, invalid("unexpected upstream task ID")
			}
			switch out["status"] {
			case "completed":
				out, ok = out["result"].(map[string]any)
				if !ok {
					return nil, errors.New("missing upstream task result")
				}
			case "working":
				delay := time.Second
				if ms, ok := out["pollIntervalMs"].(json.Number); ok {
					if n, e := ms.Int64(); e == nil && n >= 0 {
						delay = time.Duration(min(n, int64(3600000))) * time.Millisecond
					}
				}
				timer := time.NewTimer(max(delay, 100*time.Millisecond))
				select {
				case <-ctx.Done():
					timer.Stop()
					cancelTask()
					return nil, ctx.Err()
				case <-timer.C:
				}
				continue
			case "input_required":
				cancelTask()
				return nil, errors.New("upstream task requires an interactive caller")
			case "failed", "cancelled":
				return nil, errors.New("upstream task " + out["status"].(string))
			default:
				return nil, errors.New("invalid upstream task state")
			}
			break
		}
	}
	if out["resultType"] != "complete" {
		return nil, errors.New("upstream call requires an interactive client")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	var result mcp.CallToolResult
	err = json.Unmarshal(raw, &result)
	return &result, err
}
