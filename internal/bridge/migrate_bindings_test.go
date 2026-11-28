package bridge

import (
	"context"
	"reflect"
	"testing"

	"newmcp/mcp-bridge/internal/pyrunner"
)

func TestStoredStepsMigrateToPython(t *testing.T) {
	b := testBridge(t)
	old := Object{"name": "old", "steps": []Object{
		{"id": "one", "tool_id": "qa/list", "arguments": Object{"name": Object{"$pointer": "/input/a~1b"}}},
		{"id": "two", "tool_id": "qa/list", "arguments": Object{"name": Object{"$pointer": "/steps/one/rows/0/name"}}},
	}, "result_pointer": "/steps/two/rows", "fields": []string{"name"}, "limit": 1}
	if err := b.store.Put("admin", "binding", "old", old, 0); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.migrateBindings(); err != nil {
			t.Fatal(err)
		}
	}
	var stored storedBinding
	rev, err := b.store.Get("binding", "old", &stored)
	def := stored.Binding
	if err == nil {
		def.Script, err = b.readScript(stored.Hash)
	}
	if err != nil || rev != 1 || def.Runtime != "python" || !reflect.DeepEqual(def.Uses, []string{"qa/list"}) {
		t.Fatalf("migration: revision=%d deps=%v err=%v", rev, def.Uses, err)
	}
	var calls []string
	result, err := pyrunner.Run(t.Context(), def.Script, Object{"a/b": "first"}, false, func(_ context.Context, id string, params Object) (any, error) {
		if id != "qa/list" {
			t.Fatalf("unexpected tool %s", id)
		}
		calls = append(calls, params["name"].(string))
		return Object{"rows": []Object{{"name": "next", "private": true}, {"name": "last"}}}, nil
	})
	if err != nil || !reflect.DeepEqual(calls, []string{"first", "next"}) || !reflect.DeepEqual(result, []any{Object{"name": "next"}}) {
		t.Fatalf("result=%v calls=%v err=%v", result, calls, err)
	}
}
