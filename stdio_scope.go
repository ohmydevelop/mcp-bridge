package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"

	"newmcp/mcp-bridge/internal/mcpinfo"
)

// Keep unrelated MCP frames byte-for-byte, including server-initiated call responses.
// RawMessage preserves arbitrary JSON numbers in tool arguments and request IDs.
func scopedFrame(line []byte, scope string) []byte {
	type fields = map[string]json.RawMessage
	var req, params, args fields
	var method, name string
	if json.Unmarshal(line, &req) != nil || json.Unmarshal(req["method"], &method) != nil || method != "tools/call" {
		return line
	}
	if json.Unmarshal(req["params"], &params) != nil || json.Unmarshal(params["name"], &name) != nil || (name != "call_tool" && name != mcpinfo.ExecuteScript) {
		return line
	}
	if json.Unmarshal(params["arguments"], &args) != nil || args == nil || args["task_scope"] != nil {
		return line
	}
	args["task_scope"], _ = json.Marshal(scope)
	params["arguments"], _ = json.Marshal(args)
	req["params"], _ = json.Marshal(params)
	raw, _ := json.Marshal(req)
	return append(raw, '\n')
}

func forwardScoped(input io.Reader, output io.Writer, scope string) error {
	reader := bufio.NewReader(input)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if _, e := output.Write(scopedFrame(line, scope)); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
