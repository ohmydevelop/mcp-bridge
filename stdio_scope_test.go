package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestScopedStdioFrames(t *testing.T) {
	init := `{"id":1,"method":"initialize"}`
	explicit := `{"id":2,"method":"tools/call","params":{"name":"call_tool","arguments":{"task_scope":"override"}}}`
	read := `{"id":9007199254740993,"method":"tools/call","params":{"name":"call_tool","arguments":{"arguments":{"n":9007199254740995}}}}`
	execute := strings.Replace(read, "call_tool", "bridge_execute_script", 1)
	var out bytes.Buffer
	if err := forwardScoped(strings.NewReader(strings.Join([]string{init, explicit, read, execute}, "\n")), &out, "task-a"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 || lines[0] != init || lines[1] != explicit || !json.Valid([]byte(lines[2])) {
		t.Fatal(lines)
	}
	for _, part := range []string{`"task_scope":"task-a"`, `"id":9007199254740993`, `"n":9007199254740995`} {
		if !strings.Contains(lines[2], part) || !strings.Contains(lines[3], part) {
			t.Fatal(lines[2])
		}
	}
}
