package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

const repeatedReadEvent = "tool.repeated_read"

const taskReadEvent = "task.repeated_read"

const triggerCompleted = "trigger.run.completed"

var builtinEvents = []string{"source.added", repeatedReadEvent, taskReadEvent, triggerCompleted}
var validTaskScope = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func scopedContext(ctx context.Context, explicit string, meta map[string]any) (context.Context, error) {
	scope, err := readScope(explicit, meta)
	if scope != "" {
		ctx = context.WithValue(ctx, taskReadScopeKey{}, scope)
	}
	return ctx, err
}

func ValidTaskScope(scope string) bool { return validTaskScope.MatchString(scope) }

// Explicit scopes cannot contain ':', keeping host attribution in a separate namespace.
func readScope(explicit string, meta map[string]any) (string, error) {
	if explicit != "" {
		if !ValidTaskScope(explicit) {
			return "", errors.New("invalid task_scope: use 1-128 letters, digits, dots, underscores or hyphens")
		}
		return explicit, nil
	}
	turnMeta, _ := meta["x-codex-turn-metadata"].(map[string]any)
	thread, _ := turnMeta["thread_id"].(string)
	turn, _ := turnMeta["turn_id"].(string)
	if top, ok := meta["threadId"]; ok {
		id, ok := top.(string)
		if _, nested := turnMeta["thread_id"]; !ok || (nested && thread != id) {
			return "", nil
		}
		thread = id
	}
	if !ValidTaskScope(thread) || !ValidTaskScope(turn) {
		return "", nil
	}
	return "codex:" + thread + ":" + turn, nil
}

// Count completed reads in the caller's task. Persist the second observation
// and its event in one transaction, including when multiple calls finish concurrently.
func (b *Bridge) recordTaskRead(ctx context.Context, actor, scope, tool string, args any) error {
	raw, err := json.Marshal([]any{actor, scope, tool, args})
	if err != nil {
		return err
	}
	signature := digest(raw)
	tx, err := b.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM task_reads WHERE seen < unixepoch()-86400"); err != nil {
		return err
	}
	var hits int
	err = tx.QueryRow(`INSERT INTO task_reads VALUES (?,1,unixepoch())
    ON CONFLICT(signature) DO UPDATE SET hits=hits+1,seen=unixepoch() RETURNING hits`, signature).Scan(&hits)
	if err != nil {
		return err
	}
	if hits == 2 {
		ev := Occurrence{ID: taskReadEvent + "/" + signature, Name: taskReadEvent, Data: Object{
			"actor": actor, "task_scope": scope, "tool_id": tool, "readonly_calls": hits, "repeated_reads": hits - 1,
		}}
		if err = b.appendEventsTx(tx, "bridge.builtin", []Occurrence{ev}, "", nil); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Each composition runs its Python callbacks sequentially.
type bindingReads struct {
	calls   int
	reads   int
	repeats int
	seen    map[string]bool
}

func (m *bindingReads) record(tool string, args any, readOnly bool) {
	m.calls++
	if !readOnly {
		return
	}
	m.reads++
	raw, err := json.Marshal(args)
	if err != nil {
		return
	}
	key := tool + "/" + digest(raw)
	if m.seen[key] {
		m.repeats++
		return
	}
	if m.seen == nil {
		m.seen = make(map[string]bool)
	}
	m.seen[key] = true
}

func (b *Bridge) reportReads(ctx context.Context, toolID string, revision int, start time.Time, m *bindingReads, script string) {
	if m.repeats == 0 {
		return
	}
	// The durable event uniqueness constraint coalesces concurrent observations and replay runs.
	key := strings.TrimPrefix(toolID, "bridge.builtin/")
	if revision == 0 {
		// One-off scripts share an entry point, but have independent observations.
		key += "/" + digest([]byte(script))
	}
	id := fmt.Sprintf("%s/%s/%d/%d", repeatedReadEvent, key, revision, start.Unix()/3600)
	data := Object{"tool_id": toolID, "revision": revision, "upstream_calls": m.calls,
		"readonly_calls": m.reads, "repeated_reads": m.repeats, "elapsed_ms": time.Since(start).Milliseconds()}
	if err := b.appendEvents(ctx, "bridge.builtin", []Occurrence{{ID: id, Name: repeatedReadEvent, Data: data}}, "", nil); err != nil {
		log.Printf("failed to record repeated reads for %s: %v", toolID, err)
	}
}
