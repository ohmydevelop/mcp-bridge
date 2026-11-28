package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var scriptHash = regexp.MustCompile("^[0-9a-f]{64}$")

// The outer field shadows Binding.Script only when serializing for storage.
// Callers of bridge_get_tool continue to receive the full definition.
type bindingSnapshot struct {
	Binding
	Hash       string  `json:"source_hash"`
	OmitSource *string `json:"script,omitempty"`
}

func (b *Bridge) scriptDir() (string, error) {
	dir := filepath.Join(b.dir, "scripts")
	info, err := os.Lstat(dir)
	if err != nil {
		return dir, err
	}
	if !info.IsDir() {
		return dir, fmt.Errorf("expected private script directory: %s", dir)
	}
	return dir, nil
}

func (b *Bridge) readScript(hash string) (string, error) {
	if !scriptHash.MatchString(hash) {
		return "", errors.New("invalid script digest")
	}
	dir, err := b.scriptDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, hash+".py")
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular script snapshot: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if digest(data) != hash {
		return "", fmt.Errorf("script checksum mismatch: %s", path)
	}
	return string(data), nil
}

func (b *Bridge) snapshot(def Binding) (bindingSnapshot, error) {
	record := bindingSnapshot{Binding: def, Hash: digest([]byte(def.Script))}
	record.Binding.Script = ""
	if def.Script == "" {
		return record, errors.New("missing script source")
	}
	if _, err := b.readScript(record.Hash); !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	dir := filepath.Join(b.dir, "scripts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return record, err
	}
	if err := syncScriptDir(b.dir); err != nil {
		return record, err
	}
	f, err := os.CreateTemp(dir, ".stage-")
	if err != nil {
		return record, err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(def.Script); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return record, err
	}
	if closeErr != nil {
		return record, closeErr
	}
	if err = publishScriptFile(f.Name(), filepath.Join(dir, record.Hash+".py")); err != nil {
		return record, err
	}
	_, err = b.readScript(record.Hash)
	return record, err
}

func (b *Bridge) publishBinding(ctx context.Context, actor string, def Binding, rev int) error {
	tx, err := b.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = b.putBinding(tx, actor, def, rev); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *Bridge) putBinding(tx *sql.Tx, actor string, def Binding, rev int) error {
	record, err := b.snapshot(def)
	if err != nil {
		return err
	}
	if err = b.store.PutTx(tx, actor, "binding", def.ID, record, rev); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO script_versions(id,revision,hash) SELECT id,revision,? FROM objects WHERE kind='binding' AND id=?", record.Hash, def.ID)
	return err
}

func (b *Bridge) pruneScripts(ctx context.Context) error {
	b.modify.Lock()
	defer b.modify.Unlock()
	b.scriptsMu.Lock()
	defer b.scriptsMu.Unlock()
	tx, err := b.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	candidates := map[toolVersion]bool{}
	rows, err := tx.QueryContext(ctx, "SELECT id,revision FROM script_versions WHERE retired_at<=unixepoch()-30*86400")
	if err != nil {
		return err
	}
	for rows.Next() {
		var ref toolVersion
		if err = rows.Scan(&ref.ID, &ref.Revision); err != nil {
			rows.Close()
			return err
		}
		ref.ID = "bridge.builtin/" + ref.ID
		candidates[ref] = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if len(candidates) > 0 {
		// Handler references live inside encrypted flow definitions and run snapshots.
		// Only inspect them when there are old versions eligible for reclamation.
		for _, q := range []struct{ kind, query string }{
			{"flow", "SELECT id,data FROM objects WHERE kind='flow'"},
			{"run", "SELECT id,input FROM trigger_runs"},
		} {
			rows, err = tx.QueryContext(ctx, q.query)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id string
				var data []byte
				var pin struct {
					Handler *toolVersion `json:"handler"`
				}
				if err = rows.Scan(&id, &data); err == nil {
					err = b.store.Decode(q.kind, id, data, &pin)
				}
				if err != nil {
					rows.Close()
					return err
				}
				if pin.Handler != nil {
					delete(candidates, *pin.Handler)
				}
				if len(candidates) == 0 {
					break
				}
			}
			if err = rows.Err(); err != nil {
				return err
			}
			rows.Close()
			if len(candidates) == 0 {
				break
			}
		}
		for ref := range candidates {
			id := strings.TrimPrefix(ref.ID, "bridge.builtin/")
			for _, query := range []string{"DELETE FROM binding_versions WHERE id=? AND revision=?", "DELETE FROM script_versions WHERE id=? AND revision=?"} {
				if _, err = tx.ExecContext(ctx, query, id, ref.Revision); err != nil {
					return err
				}
			}
		}
	}
	rows, err = tx.QueryContext(ctx, "SELECT DISTINCT hash FROM script_versions")
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for rows.Next() {
		var hash string
		if err = rows.Scan(&hash); err != nil {
			rows.Close()
			return err
		}
		keep[hash+".py"] = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return err
	}
	dir, err := b.scriptDir()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, f := range files {
		name := f.Name()
		hash, py := strings.CutSuffix(name, ".py")
		if !f.Type().IsRegular() || keep[name] || !(py && scriptHash.MatchString(hash) || strings.HasPrefix(name, ".stage-")) {
			continue
		}
		if err = os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
