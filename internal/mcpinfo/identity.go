package mcpinfo

import (
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const ExecuteScript = "bridge_execute_script"

var Implementation = &mcp.Implementation{Name: "mcp-bridge", Version: "0.2.0"}
var toolName = regexp.MustCompile("^[a-z][a-z0-9_]{0,63}$")

func ValidToolName(s string) bool { return toolName.MatchString(s) }
