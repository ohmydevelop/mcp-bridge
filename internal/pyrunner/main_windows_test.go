package pyrunner

import (
	"os"
	"testing"

	"newmcp/mcp-bridge/internal/platform"
)

func TestMain(m *testing.M) {
	if handled, code := platform.ProcessHelper(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
