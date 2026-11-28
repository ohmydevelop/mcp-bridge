package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestScriptSnapshotsMigrateAndCollect(t *testing.T) {
	b := testBridge(t)
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	def := Binding{ID: "report", Runtime: "python", PythonScript: PythonScript{Script: "def run(args, ctx): return 1"}}
	old := def.Script
	check(b.store.Put("admin", "binding", def.ID, def, 0))
	def.Script = "def run(args, ctx): return 2"
	check(b.store.Put("admin", "binding", def.ID, def, 1))
	check(b.migrateBindings())
	check(b.migrateBindings())
	for _, q := range []string{"SELECT data FROM objects WHERE kind='binding'", "SELECT data FROM binding_versions"} {
		rows, err := b.store.DB.Query(q)
		check(err)
		for rows.Next() {
			var data []byte
			check(rows.Scan(&data))
			var fields Object
			check(b.store.Decode("binding", def.ID, data, &fields))
			if _, exists := fields["script"]; exists || fields["source_hash"] == nil {
				t.Fatal("expected metadata-only definition", fields)
			}
		}
		check(rows.Err())
		check(rows.Close())
	}
	// A failed optimistic write can leave a file but must never publish a version.
	stale := def
	stale.Script = "def run(args, ctx): return 999"
	if err := b.publishBinding(t.Context(), "admin", stale, 1); err == nil {
		t.Fatal("accepted stale publication")
	}
	check(b.pruneScripts(t.Context()))
	if _, err := b.readScript(digest([]byte(stale.Script))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan from failed publication: %v", err)
	}
	ref := toolVersion{"bridge.builtin/report", 1}
	read, rev, err := b.readVersion(ref, Principal{"admin"})
	check(err)
	if rev != 1 || read.Script != old {
		t.Fatal("old revision not restored")
	}
	def.ID = "copy"
	def.Script = old
	check(b.publishBinding(t.Context(), "admin", def, 0))
	files, err := os.ReadDir(filepath.Join(b.dir, "scripts"))
	check(err)
	if len(files) != 2 {
		t.Fatal("identical sources were not deduplicated")
	}
	check(b.store.Put("admin", "binding", def.ID, nil, 1))
	_, err = b.store.DB.Exec("UPDATE script_versions SET retired_at=1 WHERE retired_at IS NOT NULL")
	check(err)
	flow := Flow{ID: "paused", Disabled: true, Handler: &ref}
	check(b.store.Put("admin", "flow", flow.ID, flow, 0))
	check(b.pruneScripts(t.Context()))
	_, _, err = b.readVersion(ref, Principal{"admin"})
	check(err)
	raw, err := b.store.Seal("run", "queued", invocation{Handler: &ref})
	check(err)
	_, err = b.store.DB.Exec("INSERT INTO trigger_runs(id,status,input) VALUES('queued','pending',?)", raw)
	check(err)
	check(b.store.Put("admin", "flow", flow.ID, nil, 1))
	check(b.pruneScripts(t.Context()))
	_, _, err = b.readVersion(ref, Principal{"admin"})
	check(err)
	_, err = b.store.DB.Exec("DELETE FROM trigger_runs WHERE id='queued'")
	check(err)
	check(b.pruneScripts(t.Context()))
	if _, err = b.readScript(digest([]byte(old))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced old source retained: %v", err)
	}
	ref.Revision = 2
	read, _, err = b.readVersion(ref, Principal{"admin"})
	check(err)
	file := filepath.Join(b.dir, "scripts", digest([]byte(read.Script))+".py")
	check(os.WriteFile(file, []byte("changed"), 0600))
	if _, _, err = b.readVersion(ref, Principal{"admin"}); err == nil {
		t.Fatal("accepted modified snapshot")
	}
	if err = b.migrateBindings(); err == nil {
		t.Fatal("accepted damaged restore")
	}
}
