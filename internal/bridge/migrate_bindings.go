package bridge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"database/sql"
	"errors"
)

// Translate persisted definitions once. Only Python is accepted at publication or execution.
type storedBinding struct {
	Binding
	Hash  string `json:"source_hash,omitempty"`
	Steps []struct {
		ID   string `json:"id"`
		Tool string `json:"tool_id"`
		Args Object `json:"arguments"`
	} `json:"steps"`
	Result string   `json:"result_pointer"`
	Limit  int      `json:"limit"`
	Fields []string `json:"fields"`
}

func (b *Bridge) migrateBindings() error {
	tx, err := b.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lastID string
	var lastRevision int
	// Each version is migrated once, including unpublished and superseded tools.
	for {
		var id string
		var revision int
		var current bool
		var data []byte
		err = tx.QueryRow(`SELECT v.id,v.revision,v.data,EXISTS(
			SELECT 1 FROM objects o WHERE o.kind='binding' AND o.id=v.id AND o.revision=v.revision)
			FROM binding_versions v LEFT JOIN script_versions s USING(id,revision)
			WHERE s.id IS NULL AND (v.id,v.revision)>(?,?) ORDER BY v.id,v.revision LIMIT 1`, lastID, lastRevision).Scan(&id, &revision, &data, &current)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		lastID, lastRevision = id, revision
		var old storedBinding
		if err = b.store.Decode("binding", id, data, &old); err != nil {
			return err
		}
		def := old.Binding
		if old.Runtime != "python" {
			if len(old.Steps) == 0 || (old.Runtime != "" && old.Runtime != "steps") {
				return fmt.Errorf("unsupported stored binding %q", id)
			}
			def = old.python()
		}
		record, err := b.snapshot(def)
		if err != nil {
			return err
		}
		raw, err := b.store.Seal("binding", id, record)
		if err != nil {
			return err
		}
		if current {
			// Updating the live object archives its new representation at the same revision.
			if _, err = tx.Exec("DELETE FROM binding_versions WHERE id=? AND revision=?", id, revision); err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE objects SET data=? WHERE kind='binding' AND id=?", raw, id); err != nil {
				return err
			}
		} else if _, err = tx.Exec("UPDATE binding_versions SET data=? WHERE id=? AND revision=?", raw, id, revision); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO script_versions VALUES(?,?,?,CASE WHEN ? THEN NULL ELSE unixepoch() END)", id, revision, record.Hash, current); err != nil {
			return err
		}
	}
	// Fail before serving if a restored state directory omits or corrupts a snapshot.
	rows, err := tx.Query("SELECT DISTINCT hash FROM script_versions")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		if err = rows.Scan(&hash); err != nil {
			return err
		}
		if _, err = b.readScript(hash); err != nil {
			return fmt.Errorf("verify managed script %s: %w", hash, err)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// Values have already been decoded from JSON. Quote the serialized JSON as a Python string.
func pythonJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return "json.loads(" + strconv.Quote(string(data)) + ")"
}

func (old storedBinding) python() Binding {
	def := old.Binding
	def.Runtime = "python"
	var code strings.Builder
	code.WriteString(`import json
def run(args, ctx):
    values = {"input": args, "steps": {}}
    def select(path):
        value = values
        if path and not path.startswith("/"):
            raise ValueError("expected JSON pointer")
        for token in path.split("/")[1:]:
            key = token.replace("~1", "/").replace("~0", "~")
            if isinstance(value, list):
                key = int(key)
                if key < 0:
                    raise IndexError(key)
            elif not isinstance(value, dict):
                raise ValueError("invalid pointer traversal")
            value = value[key]
        return value
`)
	for _, step := range old.Steps {
		if !member(def.Uses, step.Tool) {
			def.Uses = append(def.Uses, step.Tool)
		}
		code.WriteString("    params = {}\n")
		for key, v := range step.Args {
			expr := pythonJSON(v)
			if obj, ok := v.(Object); ok {
				if p, ok := obj["$pointer"].(string); ok {
					expr = "select(" + pythonJSON(p) + ")"
				}
			}
			fmt.Fprintf(&code, "    params[%s] = %s\n", pythonJSON(key), expr)
		}
		fmt.Fprintf(&code, "    row = ctx.call_tool(%s, params)\n", pythonJSON(step.Tool))
		code.WriteString("    if not isinstance(row, dict):\n        raise ValueError('expected structured upstream object')\n")
		fmt.Fprintf(&code, "    values['steps'][%s] = row\n", pythonJSON(step.ID))
	}
	fmt.Fprintf(&code, "    result = select(%s)\n", pythonJSON(old.Result))
	if old.Limit > 0 || len(old.Fields) > 0 {
		code.WriteString("    if not isinstance(result, list):\n        raise ValueError('projection requires an array')\n")
	}
	if old.Limit > 0 {
		fmt.Fprintf(&code, "    result = result[:%d]\n", old.Limit)
	}
	if len(old.Fields) > 0 {
		code.WriteString("    if not all(isinstance(row, dict) for row in result):\n        raise ValueError('field selection requires object rows')\n")
		fmt.Fprintf(&code, "    result = [{k: row[k] for k in %s if k in row} for row in result]\n", pythonJSON(old.Fields))
	}
	code.WriteString("    return result\n")
	def.Script = code.String()
	return def
}
