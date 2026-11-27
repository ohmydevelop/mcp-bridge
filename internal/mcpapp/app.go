package mcpapp

import (
	"context"
	_ "embed"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed app.html
var appHTML string

func Register(server *mcp.Server) {
	const uri = "ui://mcp-bridge/manage.html"
	const mime = "text/html;profile=mcp-app"
	server.AddResource(&mcp.Resource{Name: "Bridge management", URI: uri, MIMEType: mime}, func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Text: appHTML}}}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "bridge_open_app", Description: "Open the bridge management MCP App.", Meta: mcp.Meta{"ui": map[string]any{"resourceUri": uri}}}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Manage sources, skills and automation flows."}}}, nil, nil
	})
}
