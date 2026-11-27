package bridge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"newmcp/mcp-bridge/internal/storage"
)

// Translate persisted definitions once. Only Python is accepted at publication or execution.
type storedBinding struct {
	Binding
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
	defs, err := storage.ListTx[storedBinding](b.store, tx, "binding")
	if err != nil {
		return err
	}
	for _, old := range defs {
		if old.Runtime == "python" {
			continue
		}
		if len(old.Steps) == 0 || (old.Runtime != "" && old.Runtime != "steps") {
			return fmt.Errorf("unsupported stored binding %q", old.ID)
		}
		def := old.python()
		raw, err := b.store.Seal("binding", def.ID, def)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM binding_versions WHERE id=? AND revision=(SELECT revision FROM objects WHERE kind='binding' AND id=?)", def.ID, def.ID); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE objects SET data=? WHERE kind='binding' AND id=?", raw, def.ID); err != nil {
			return err
		}
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
