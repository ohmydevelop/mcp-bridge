package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"newmcp/mcp-bridge/internal/storage"
)

type backgroundKey struct{}
type recordedRead struct {
	Tool  string `json:"tool_id"`
	Args  Object `json:"arguments"`
	Value any    `json:"response"`
}
type executionRecord struct {
	Offline bool           `json:"-"`
	Capture bool           `json:"-"`
	Count   int            `json:"reads"`
	Reads   []recordedRead `json:"calls,omitempty"`
	used    map[int]bool
}

func (r *executionRecord) record(tool string, args Object, v any) {
	r.Count++
	if r.Capture {
		r.Reads = append(r.Reads, recordedRead{tool, args, v})
	}
}
func (r *executionRecord) read(tool string, args Object) (any, error) {
	for i, x := range r.Reads {
		if !r.used[i] && x.Tool == tool && equal(x.Args, args) {
			r.used[i] = true
			r.Count++
			return x.Value, nil
		}
	}
	return nil, errors.New("unrecorded replay call")
}

type replayCase struct {
	Arguments Object         `json:"arguments"`
	Expected  Object         `json:"expected"`
	Calls     []recordedRead `json:"calls,omitempty"`
}
type promotionPolicy struct {
	Enabled bool         `json:"enabled"`
	Metric  string       `json:"metric,omitempty"`
	Fields  []string     `json:"required_data_fields,omitempty"`
	Holdout []replayCase `json:"holdout,omitempty"`
}

func (b *Bridge) checkBackgroundTools(p Principal, def Binding) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.bindingAllowed(p, def) {
		return errors.New("unavailable dependency")
	}
	for _, ref := range def.Uses {
		s, n, _ := strings.Cut(ref, "/")
		r := b.live[s]
		if r == nil || r.tools[n] == nil || r.tools[n].Annotations == nil || !r.tools[n].Annotations.ReadOnlyHint {
			return fmt.Errorf("background tool %s must be read-only", ref)
		}
	}
	return nil
}
func (b *Bridge) handleInput(ctx context.Context, p Principal, def Binding, rev int, args Object, record *executionRecord) (Object, error) {
	schema, err := inputSchema(def.Schema)
	if err != nil {
		return nil, err
	}
	if err = schema.Validate(args); err != nil {
		return nil, err
	}
	result, err := b.executePython(context.WithValue(ctx, backgroundKey{}, record), p, def.PythonScript, args, "bridge.builtin/"+def.ID, rev)
	if err != nil {
		return nil, err
	}
	out, _ := result.StructuredContent.(Object)["result"].(Object)
	switch out["action"] {
	case "notify":
		if _, ok := out["data"]; ok {
			return out, nil
		}
	case "skip":
		if reason, _ := out["reason"].(string); reason != "" {
			return out, nil
		}
	}
	return nil, errors.New("expected notify with data or skip with reason")
}
func (b *Bridge) processRun(ctx context.Context) error {
	var id, owner string
	var raw []byte
	err := b.store.DB.QueryRowContext(ctx, "SELECT id,owner,input FROM trigger_runs WHERE status='pending' ORDER BY rowid LIMIT 1").Scan(&id, &owner, &raw)
	if err != nil {
		return err
	}
	var in invocation
	if err = b.store.Decode("run", id, raw, &in); err != nil {
		return err
	}
	if in.Handler == nil {
		return errors.New("missing handler")
	}
	p := Principal{owner}
	rec := executionRecord{Capture: in.Replay}
	def, rev, err := b.readVersion(*in.Handler, p)
	var out Object
	if err == nil {
		out, err = b.handleInput(ctx, p, def, rev, in.Args, &rec)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	state := "processed"
	summary := Object{"reads": rec.Count}
	if in.Replay {
		summary["calls"] = rec.Reads
	}
	if err != nil {
		state = "failed"
		summary["error"] = err.Error()
	} else {
		summary["result"] = out
		payload, _ := json.Marshal(out)
		summary["output_bytes"] = len(payload)
		if out["action"] == "skip" {
			state = "skipped"
		}
	}
	raw, err = b.store.Seal("out", id, summary)
	if err != nil {
		return err
	}
	tx, err := b.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.Exec("UPDATE trigger_runs SET status=?,output=? WHERE id=? AND status='pending'", state, raw, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if state == "processed" {
		trigger := in.Args["trigger_id"].(string)
		body := in.deliveryBody(id, trigger)
		body["data"] = out["data"]
		if err = b.enqueueDelivery(tx, id, trigger, in.Destination, body); err != nil {
			return err
		}
	}
	if in.Replay {
		source, _ := in.Args["source_id"].(string)
		event, _ := in.Args["event"].(Object)
		if source != "bridge.builtin" || event["name"] != triggerCompleted {
			notice := Occurrence{ID: "completed/" + id, Name: triggerCompleted, Data: Object{"run_id": id, "trigger_id": in.Args["trigger_id"], "actor": owner, "status": state, "reads": rec.Count, "handler": in.Handler}}
			if err = b.appendEventsTx(tx, "bridge.builtin", []Occurrence{notice}, "", nil); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func (b *Bridge) reviewCandidates(ctx context.Context) error {
	flows, err := storage.List[Flow](b.store, "flow")
	if err != nil {
		return err
	}
	var firstError error
	for _, f := range flows {
		if f.Disabled || f.Handler == nil || f.Promotion == nil || !f.Promotion.Enabled {
			continue
		}
		if err = b.reviewCandidate(ctx, f); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}
func (b *Bridge) reviewCandidate(ctx context.Context, f Flow) error {
	p := Principal{f.Owner}
	var active Flow
	config, err := b.store.Get("flow", f.ID, &active)
	if err != nil || !equal(f, active) {
		return err
	}
	def, rev, err := b.readVersion(toolVersion{ID: f.Handler.ID}, p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if rev <= f.Handler.Revision {
		return nil
	}
	var samples, done int
	err = b.store.DB.QueryRow("SELECT count(*) FROM trigger_runs WHERE instance=? AND status IN ('processed','skipped') AND output IS NOT NULL", f.Instance).Scan(&samples)
	if err != nil {
		return err
	}
	err = b.store.DB.QueryRow("SELECT count(*) FROM trigger_reviews WHERE instance=? AND candidate=? AND config=? AND samples=?", f.Instance, rev, config, samples).Scan(&done)
	if err != nil || done != 0 {
		return err
	}
	decision, err := b.evaluate(ctx, f, def, rev)
	if err != nil {
		return err
	}
	return b.commitReview(ctx, f, config, def.ID, rev, samples, decision)
}

func (b *Bridge) commitReview(ctx context.Context, f Flow, config int, tool string, rev int, samples int, decision string) error {
	b.modify.Lock()
	defer b.modify.Unlock()
	var now Flow
	current, err := b.store.Get("flow", f.ID, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || config != current || now.Instance != f.Instance {
		return err
	}
	tx, err := b.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if decision == "promoted" {
		var latest int
		if err = tx.QueryRow("SELECT revision FROM objects WHERE kind='binding' AND id=?", tool).Scan(&latest); err != nil {
			return err
		}
		if latest != rev {
			return nil
		}
		now.Handler = &toolVersion{f.Handler.ID, rev}
		if err = b.store.PutTx(tx, f.Owner, "flow", f.ID, now, current); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO trigger_reviews VALUES(?,?,?,?,?)", f.Instance, rev, config, samples, decision); err != nil {
		return err
	}
	return tx.Commit()
}
func (b *Bridge) evaluate(ctx context.Context, f Flow, def Binding, rev int) (string, error) {
	p := Principal{f.Owner}
	old, _, err := b.readVersion(*f.Handler, p)
	if err != nil {
		return "", err
	}
	if err = b.checkBackgroundTools(p, def); err != nil {
		return "unavailable dependency", nil
	}
	rows, err := b.store.DB.Query("SELECT id,input,output FROM trigger_runs WHERE instance=? AND output IS NOT NULL AND status IN ('processed','skipped') ORDER BY rowid DESC LIMIT 100", f.Instance)
	if err != nil {
		return "", err
	}
	var cases []replayCase
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		var raw, out []byte
		var in invocation
		var rec struct {
			Result Object
			Calls  []recordedRead
		}
		if err = rows.Scan(&id, &raw, &out); err == nil {
			err = b.store.Decode("run", id, raw, &in)
		}
		if err == nil {
			err = b.store.Decode("out", id, out, &rec)
		}
		if err != nil {
			rows.Close()
			return "", err
		}
		if !in.Replay || in.Handler == nil || *in.Handler != *f.Handler {
			continue
		}
		cases = append(cases, replayCase{in.Args, rec.Result, rec.Calls})
		seen[caseKey(in.Args)] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	if len(cases) == 0 {
		return "no baseline samples", nil
	}
	for _, h := range f.Promotion.Holdout {
		key := caseKey(h.Arguments)
		if seen[key] {
			return "duplicate holdout", nil
		}
		seen[key] = true
		cases = append(cases, h)
	}
	var improved bool
	for _, s := range cases {
		cost := func(d Binding, version int) (int, bool) {
			record := executionRecord{Offline: true, Reads: s.Calls, used: map[int]bool{}}
			out, err := b.handleInput(ctx, p, d, version, s.Arguments, &record)
			if err != nil || !validReplayOutput(out, s.Expected, f.Promotion.Fields) {
				return 0, false
			}
			if f.Promotion.Metric == "reads" {
				return record.Count, true
			}
			raw, _ := json.Marshal(out)
			return len(raw), true
		}
		a, ok := cost(old, f.Handler.Revision)
		if !ok {
			return "baseline mismatch", nil
		}
		c, ok := cost(def, rev)
		if !ok {
			return "candidate mismatch or unrecorded read", nil
		}
		if c > a {
			return "metric regression", nil
		}
		improved = improved || c < a
	}
	if !improved {
		return "no improvement", nil
	}
	return "promoted", nil
}
func caseKey(args Object) string {
	input := Object{}
	for k, v := range args {
		if k != "run_id" && k != "trigger_id" {
			input[k] = v
		}
	}
	raw, _ := json.Marshal(input)
	return digest(raw)
}

// By default the entire result is authoritative. Businesses may instead declare
// the required data fields to allow candidates to trim oversized notifications.
func validReplayOutput(out, expected Object, fields []string) bool {
	if len(fields) == 0 || expected["action"] != "notify" {
		return equal(out, expected)
	}
	if out["action"] != "notify" {
		return false
	}
	want, _ := expected["data"].(Object)
	got, _ := out["data"].(Object)
	for _, key := range fields {
		w, ok := want[key]
		v, present := got[key]
		if !ok || !present || !equal(v, w) {
			return false
		}
	}
	return true
}
