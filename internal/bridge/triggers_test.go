package bridge

import (
	"bytes"

	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTriggerLifecycle(t *testing.T) {
	b := testBridge(t)
	cs := clientFor(t, b, Principal{"operator"})
	invoke := func(name string, args Object, fail bool) Object {
		t.Helper()
		result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError != fail {
			t.Fatalf("%s: %v %+v", name, err, result)
		}
		if fail {
			return nil
		}
		return result.StructuredContent.(map[string]any)
	}
	var bodies []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if len(bodies) == 1 {
			w.WriteHeader(503)
		}
	}))
	defer receiver.Close()
	t.Setenv("BRIDGE_WEBHOOK_ORIGINS", receiver.URL)
	invoke("bridge_save_destination", Object{"destination": Object{"id": "notify", "url": receiver.URL}, "revision": 0}, false)
	save := func(script string, rev int) Object {
		return invoke("bridge_save_tool", Object{"name": "summarize", "description": "Temperature reading", "input_schema": Object{"type": "object"}, "uses": []string{}, "script": script, "revision": rev}, false)
	}
	baseline := "def run(args, ctx):\n    return {'action':'notify','data':{'celsius':23, 'debug':'extra details'}}"
	candidate := "def run(args, ctx):\n    return {'action':'notify','data':{'celsius':23}}"
	ref := save(baseline, 0)
	schedule := Object{"type": "schedule", "cron": "0 9 * * *", "timezone": "Asia/Shanghai"}
	input := Object{"trigger_id": "morning", "when": schedule, "handler": ref, "destination_id": "notify", "instruction": "Summarize the reading.", "replay": true,
		"promotion": Object{"enabled": true, "metric": "output_bytes", "required_data_fields": []string{"celsius"}, "holdout": []Object{{"arguments": Object{"scheduled_at": "2025-01-01T01:00:00Z"}, "expected": Object{"action": "notify", "data": Object{"celsius": 23}}}}}}
	invoke("save_trigger", input, false)
	now := time.Date(2026, 3, 7, 4, 0, 0, 0, time.UTC)
	var f Flow
	rev, e := b.store.Get("flow", "morning", &f)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.store.DB.Exec("UPDATE trigger_clock SET checked=?", now.AddDate(0, 0, -8).Unix()); e != nil {
		t.Fatal(e)
	}
	check := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	check(b.scheduleDue(now))
	check(b.scheduleDue(now))
	invoke("save_trigger", Object{"trigger_id": "morning", "revision": rev, "instruction": ""}, false)
	rev++
	check(b.processRun(t.Context()))
	if e = b.processRun(t.Context()); !errors.Is(e, sql.ErrNoRows) {
		t.Fatalf("duplicate schedule: %v", e)
	}
	var id string
	check(b.store.DB.QueryRow("SELECT id FROM trigger_runs").Scan(&id))
	detail := invoke("get_trigger", Object{"trigger_id": "morning", "run_id": id}, false)
	if detail["arguments"].(Object)["scheduled_at"] != "2026-03-07T01:00:00Z" {
		t.Fatal(detail)
	}
	check(b.deliver(t.Context()))
	_, e = b.store.DB.Exec("UPDATE deliveries SET next=0")
	check(e)
	check(b.deliver(t.Context()))
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatal(bodies)
	}
	var delivered Object
	check(json.Unmarshal([]byte(bodies[0]), &delivered))
	if delivered["instruction"] != "Summarize the reading." {
		t.Fatal(delivered)
	}
	if current := invoke("get_trigger", Object{"trigger_id": "morning"}, false)["trigger"].(Object); current["instruction"] != "" {
		t.Fatal(current)
	}
	// A wrong answer cannot be promoted, even if it is shorter.
	save("def run(args, ctx):\n    return {'action':'notify','data':{'celsius':0}}", 1)
	check(b.reviewCandidates(t.Context()))
	state := invoke("get_trigger", Object{"trigger_id": "morning"}, false)
	if state["review"].(Object)["decision"] == "promoted" {
		t.Fatal(state)
	}
	save(candidate, 2)
	check(b.reviewCandidates(t.Context()))
	state = invoke("get_trigger", Object{"trigger_id": "morning"}, false)
	if state["review"].(Object)["decision"] != "promoted" {
		t.Fatal(state)
	}
	if _, e = b.store.Get("flow", "morning", &f); e != nil || f.Handler.Revision != 3 {
		t.Fatal(f, e)
	}
	// Roll back without re-enabling automatic promotion.
	invoke("save_trigger", Object{"trigger_id": "morning", "revision": rev + 1, "handler": ref, "promotion": Object{"enabled": false}}, false)
	invoke("save_trigger", Object{"trigger_id": "morning", "revision": rev + 2, "disabled": true}, false)
	invoke("delete_trigger", Object{"trigger_id": "morning", "revision": rev + 3}, false)
	invoke("get_trigger", Object{"trigger_id": "morning", "run_id": id}, false)
	invoke("save_trigger", input, false)
	invoke("delete_trigger", Object{"trigger_id": "morning", "revision": 1}, true)
	fresh := invoke("get_trigger", Object{"trigger_id": "morning"}, false)
	if len(fresh["runs"].([]any)) != 0 || len(fresh["deliveries"].([]any)) != 0 {
		t.Fatal(fresh)
	}
}

func TestMigrateExistingTrigger(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{8}, 32)
	legacy, err := openBridge(t.Context(), dir, key)
	if err != nil {
		t.Fatal(err)
	}
	f := Flow{ID: "old", Source: "bridge.builtin", Name: "source.added", Owner: "operator", Destination: "alerts"}
	if err = legacy.store.Put("operator", "flow", f.ID, f, 0); err != nil {
		t.Fatal(err)
	}
	if err = legacy.store.Put("operator", "cursor", f.ID, "cursor-1", 0); err != nil {
		t.Fatal(err)
	}
	tx, err := legacy.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	oldBody := Object{"delivery_id": "original", "flow_id": "old", "source_id": f.Source, "event": Object{"name": f.Name}, "skill_ref": ""}
	if err = legacy.enqueueDelivery(tx, "original", f.ID, f.Destination, oldBody); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = legacy.store.DB.Exec(`DROP TRIGGER remember_revision; DROP TRIGGER archive_binding_insert; DROP TRIGGER archive_binding_update;
 DROP TABLE retired_revisions; DROP TABLE binding_versions; DROP TABLE trigger_runs; DROP TABLE trigger_reviews; DROP TABLE trigger_clock; DROP TABLE local_deliveries; PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = legacy.store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := openBridge(t.Context(), dir, key)
	if err != nil {
		t.Fatal(err)
	}
	defer b.store.DB.Close()
	for range 2 {
		if err := b.migrateTriggers(); err != nil {
			t.Fatal(err)
		}
	}
	var cursor string
	if _, err := b.store.Get("cursor", f.ID, &cursor); err != nil || cursor != "cursor-1" {
		t.Fatal(cursor, err)
	}
	if rev, err := b.store.Get("flow", f.ID, &f); rev != 1 || err != nil || f.Instance == "" {
		t.Fatal(rev, f, err)
	}
	var raw []byte
	var body Object
	if err = b.store.DB.QueryRow("SELECT data FROM deliveries WHERE id='original'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err = b.store.Decode("delivery", "original", raw, &body); err != nil || !equal(oldBody, body) {
		t.Fatal(body, err)
	}
	var n int
	if err := b.store.DB.QueryRow("SELECT COUNT(*) FROM deliveries WHERE flow='old' AND status='pending'").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestPinnedRunSkipFailureAndUnrecordedRead(t *testing.T) {
	b := testBridge(t)
	p := Principal{"operator"}
	definition := Binding{ID: "decide", Runtime: "python", Schema: Object{"type": "object"}, PythonScript: PythonScript{
		Script: "def run(args, ctx):\n    return {'action':'skip','reason':'too cold'}"}}
	put := func(def Binding, rev int) {
		t.Helper()
		if err := b.store.Put(p.Name, "binding", def.ID, def, rev); err != nil {
			t.Fatal(err)
		}
	}
	put(definition, 0)
	f := Flow{ID: "sensor", Instance: "generation", Owner: p.Name, Name: "reading", Source: "sensor", Destination: "alerts", Handler: &toolVersion{"bridge.builtin/decide", 1}}
	if err := b.store.Put(p.Name, "flow", f.ID, f, 0); err != nil {
		t.Fatal(err)
	}
	emit := func(id string) {
		t.Helper()
		if err := b.appendEvents(t.Context(), f.Source, []Occurrence{{ID: id, Name: f.Name, Data: Object{"value": 2}}}, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	emit("1")
	emit("1")
	definition.Script = "def run(args, ctx):\n    raise ValueError('missing value')"
	put(definition, 1)
	f.Handler.Revision = 2
	if err := b.store.Put(p.Name, "flow", f.ID, f, 1); err != nil {
		t.Fatal(err)
	}
	emit("2")
	for _, state := range []string{"skipped", "failed"} {
		if err := b.processRun(t.Context()); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := b.store.DB.QueryRow("SELECT COUNT(*) FROM trigger_runs WHERE status=?", state).Scan(&n); n != 1 || err != nil {
			t.Fatal(state, n, err)
		}
	}
	var n int
	if err := b.store.DB.QueryRow("SELECT COUNT(*) FROM deliveries").Scan(&n); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	def := Binding{ID: "offline", Schema: Object{"type": "object"}, PythonScript: PythonScript{Script: "def run(args, ctx):\n    return {'action':'notify','data':ctx.call_tool('meter/read', {})}", Uses: []string{"meter/read"}}}
	_, err := b.handleInput(t.Context(), p, def, 1, Object{}, &executionRecord{Offline: true, used: map[int]bool{}})
	if err == nil {
		t.Fatal("accepted unrecorded replay read")
	}
}

func TestNoSamplesAndStaleSubscription(t *testing.T) {
	b := testBridge(t)
	f := Flow{ID: "meter", Instance: "second", Owner: "operator", Source: "meter", Name: "reading", Handler: &toolVersion{"bridge.builtin/meter", 1}, Promotion: &promotionPolicy{Enabled: true, Metric: "reads"}}
	if err := b.store.Put("operator", "flow", f.ID, f, 0); err != nil {
		t.Fatal(err)
	}
	old := f
	old.Instance = "first"
	if err := b.appendObserved(t.Context(), old, nil, "stale"); err == nil {
		t.Fatal("wrote a retired cursor")
	}
	def := Binding{ID: "meter", Schema: Object{"type": "object"}}
	if err := b.store.Put("operator", "binding", def.ID, def, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Put("operator", "binding", def.ID, def, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.reviewCandidates(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A validation result finishing after a user update cannot overwrite that update.
	changed := f
	changed.Disabled = true
	if err := b.store.Put("operator", "flow", f.ID, changed, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.commitReview(t.Context(), f, 1, def.ID, 2, 1, "promoted"); err != nil {
		t.Fatal(err)
	}
	var current Flow
	if rev, err := b.store.Get("flow", f.ID, &current); err != nil || rev != 2 || current.Handler.Revision != 1 {
		t.Fatal(current, rev, err)
	}
	var decision string
	if err := b.store.DB.QueryRow("SELECT result FROM trigger_reviews").Scan(&decision); err != nil || decision != "no baseline samples" {
		t.Fatal(decision, err)
	}
}

func TestCompletionPollRespectsOwnerAndAdvancesCursor(t *testing.T) {
	b := testBridge(t)
	for _, actor := range []string{"alice", "bob"} {
		e := Occurrence{ID: actor, Name: triggerCompleted, Data: Object{"actor": actor}}
		if err := b.appendEvents(t.Context(), "bridge.builtin", []Occurrence{e}, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	state, err := b.pollLocal(Principal{"bob"}, "bridge.builtin", triggerCompleted, nil, "0", 1)
	if err != nil || len(state["events"].([]Occurrence)) != 0 || state["cursor"] == "0" {
		t.Fatal(state, err)
	}
	state, err = b.pollLocal(Principal{"bob"}, "bridge.builtin", triggerCompleted, nil, state["cursor"], 1)
	if err != nil || len(state["events"].([]Occurrence)) != 1 || state["events"].([]Occurrence)[0].ID != "bob" {
		t.Fatal(state, err)
	}
}

func TestDestinationAndReceiptRetirement(t *testing.T) {
	b := testBridge(t)
	client := clientFor(t, b, Principal{"operator"})
	call := func(name string, args Object) Object {
		t.Helper()
		v, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || v.IsError {
			t.Fatalf("%s: %v %v", name, err, v.Content)
		}
		return v.StructuredContent.(map[string]any)
	}
	url := "http://127.0.0.1:8912"
	t.Setenv("BRIDGE_WEBHOOK_ORIGINS", url)
	call("bridge_save_destination", Object{"destination": Object{"id": "target", "url": url}, "revision": 0})
	schedule := Object{"type": "schedule", "cron": "0 0 1 1 *", "timezone": "UTC"}
	for _, id := range []string{"one", "two"} {
		call("save_trigger", Object{"trigger_id": id, "when": schedule, "destination_id": "target"})
	}
	run := call("run_trigger_now", Object{"trigger_id": "one"})["run_id"]
	if got := call("delete_trigger", Object{"trigger_id": "one", "revision": 1, "delete_unused_destination": true}); got["destination_cleanup"] != "referenced_by_trigger" {
		t.Fatal(got)
	}
	if got := call("delete_trigger", Object{"trigger_id": "two", "revision": 1, "delete_unused_destination": true}); got["destination_cleanup"] != "removed" {
		t.Fatal(got)
	}
	checkCount := func(table string, want int) {
		t.Helper()
		var n int
		if err := b.store.DB.QueryRow("SELECT count(*) FROM "+table+" WHERE id=?", run).Scan(&n); err != nil || n != want {
			t.Fatalf("%s %d %v", table, n, err)
		}
	}
	prune := func() {
		t.Helper()
		if err := b.pruneRetired(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	prune()
	checkCount("trigger_runs", 1)
	if _, err := b.store.DB.Exec("UPDATE retired_instances SET retired_at=1; UPDATE deliveries SET status='submitting'"); err != nil {
		t.Fatal(err)
	}
	prune()
	checkCount("deliveries", 1)
	if _, err := b.store.DB.Exec("UPDATE deliveries SET status='completed'; INSERT INTO local_deliveries(id,kind) SELECT id,'cli_process' FROM deliveries"); err != nil {
		t.Fatal(err)
	}
	prune()
	for _, table := range []string{"trigger_runs", "deliveries", "local_deliveries"} {
		checkCount(table, 0)
	}
}
