package cliadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"newmcp/mcp-bridge/internal/mcpinfo"
)

// The configured executable receives one JSON value on stdin, with a fixed argv.
// Cap stdout while it is being collected, before constructing an MCP response.
type outputBuffer struct {
	bytes.Buffer
	limit int
}

func (b *outputBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, errors.New("CLI output exceeds the configured limit")
	}
	return b.Buffer.Write(data)
}
func Serve(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("cli", flag.ContinueOnError)
	name := fs.String("name", "run_cli", "published MCP tool name")
	description := fs.String("description", "Run the configured CLI with a JSON object on stdin.", "tool description")
	timeout := fs.Duration("timeout", 30*time.Second, "invocation deadline")
	limit := fs.Int("max-output", 1<<20, "maximum stdout bytes")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	fixed := fs.Args()
	if len(fixed) == 0 || !mcpinfo.ValidToolName(*name) || *timeout <= 0 || *limit < 1 || *limit > 8<<20 {
		return errors.New("usage: mcp-bridge cli [-name tool] -- executable [fixed arguments...]")
	}
	exe, err := exec.LookPath(fixed[0])
	if err != nil {
		return err
	}
	server := mcp.NewServer(mcpinfo.Implementation, nil)
	server.AddTool(&mcp.Tool{Name: *name, Description: *description, InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		args := req.Params.Arguments
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		cmd := exec.CommandContext(ctx, exe, fixed[1:]...)
		cmd.Stdin = bytes.NewReader(args)
		cmd.Stderr = io.Discard
		var out outputBuffer
		out.limit = *limit
		// Use a writer-only view to keep io.Copy on the bounded Write path.
		cmd.Stdout = struct{ io.Writer }{&out}
		cmd.WaitDelay = time.Second
		if err := cmd.Run(); err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "CLI invocation failed: " + err.Error()}}}, nil
		}
		text := out.String()
		result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
		var obj map[string]any
		if json.Unmarshal(out.Bytes(), &obj) == nil && obj != nil {
			result.StructuredContent = obj
		}
		return result, nil
	})
	return server.Run(ctx, &mcp.IOTransport{Reader: os.Stdin, Writer: os.Stdout})
}
