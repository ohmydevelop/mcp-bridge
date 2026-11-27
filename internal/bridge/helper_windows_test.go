package bridge

import (
	"os"
	"testing"

	"newmcp/mcp-bridge/internal/platform"
)

// Handler integration tests use the test binary as their Windows job supervisor.
func TestMain(m *testing.M) {
	if handled, code := platform.ProcessHelper(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
