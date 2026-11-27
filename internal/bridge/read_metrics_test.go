package bridge

import "testing"

func TestRepeatedReads(t *testing.T) {
	var m bindingReads
	m.record("qa/list", Object{"page": 1, "size": 5}, true)
	m.record("qa/list", Object{"size": 5, "page": 2}, true)
	m.record("other/list", Object{"size": 5, "page": 1}, true)
	m.record("qa/list", Object{"size": 5, "page": 1}, false)
	m.record("qa/list", Object{"size": 5, "page": 1}, true)
	if m.calls != 5 || m.reads != 4 || m.repeats != 1 {
		t.Fatalf("expected 5 calls, 4 reads and 1 repeat, got %+v", m)
	}
}

func TestDirectReadsKeepTasksAndActorsSeparate(t *testing.T) {
	b := testBridge(t)
	ctx := t.Context()
	record := func(actor, scope string, page int) error {
		return b.recordTaskRead(ctx, actor, scope, "qa/list", Object{"page": page})
	}
	for _, err := range []error{record("a", "task1", 1), record("b", "task1", 1), record("a", "task2", 1), record("a", "task1", 2)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	check := func(want int) {
		t.Helper()
		var count int
		if err := b.store.DB.QueryRow("SELECT count(*) FROM events WHERE name=?", taskReadEvent).Scan(&count); err != nil || count != want {
			t.Fatalf("count=%d, want %d, err=%v", count, want, err)
		}
	}
	check(0)
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- record("a", "task1", 1) }()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	check(1)
	if err := record("b", "task1", 1); err != nil {
		t.Fatal(err)
	}
	check(2)
}

func TestCodexReadScope(t *testing.T) {
	nested := Object{"thread_id": "thread", "turn_id": "turn"}
	meta := Object{"threadId": "thread", "x-codex-turn-metadata": nested}
	for _, tc := range []struct {
		name, explicit string
		meta           Object
		want           string
		invalid        bool
	}{
		{"consistent", "", meta, "codex:thread:turn", false},
		{"nested only", "", Object{"x-codex-turn-metadata": nested}, "codex:thread:turn", false},
		{"top only thread", "", Object{"threadId": "thread", "x-codex-turn-metadata": Object{"turn_id": "turn"}}, "codex:thread:turn", false},
		{"explicit takes precedence", "across-turns", meta, "across-turns", false},
		{"invalid explicit", "codex:thread:turn", meta, "", true},
		{"conflicting threads", "", Object{"threadId": "another", "x-codex-turn-metadata": nested}, "", false},
		{"thread without turn", "", Object{"threadId": "thread"}, "", false},
		{"no metadata", "", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readScope(tc.explicit, tc.meta)
			if got != tc.want || (err != nil) != tc.invalid {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}
