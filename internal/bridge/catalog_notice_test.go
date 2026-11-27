package bridge

import "testing"

func TestCatalogNoticeIsAtomicAndDirected(t *testing.T) {
	b := testBridge(t)
	for _, f := range []Flow{{ID: "first", Source: "bridge.builtin", Name: taskReadEvent, Destination: "one"}, {ID: "other", Source: "bridge.builtin", Name: taskReadEvent, Destination: "two"}} {
		if err := b.store.Put("admin", "flow", f.ID, f, 0); err != nil {
			t.Fatal(err)
		}
	}
	ev := Occurrence{ID: "original", Name: taskReadEvent, Data: Object{"actor": "admin", "task_scope": "codex:thread-a:turn-a"}}
	if err := b.appendEvents(t.Context(), "bridge.builtin", []Occurrence{ev}, "", nil); err != nil {
		t.Fatal(err)
	}
	var trigger string
	if err := b.store.DB.QueryRow("SELECT id FROM deliveries WHERE flow='first'").Scan(&trigger); err != nil {
		t.Fatal(err)
	}
	def := Binding{ID: "bot_flags"}
	publish := func(id string) error { return b.publishForTrigger(t.Context(), "admin", def, 0, id) }
	if publish("unknown") == nil {
		t.Fatal("accepted unknown trigger")
	}
	if err := publish(trigger); err != nil {
		t.Fatal(err)
	}
	if publish(trigger) == nil {
		t.Fatal("expected revision conflict")
	}
	var n int
	for dest, want := range map[string]int{"one": 2, "two": 1} {
		if err := b.store.DB.QueryRow("SELECT count(*) FROM deliveries WHERE destination=?", dest).Scan(&n); err != nil || n != want {
			t.Fatalf("%s: %d %v", dest, n, err)
		}
	}
}
