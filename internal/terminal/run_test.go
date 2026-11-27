package terminal

import (
	"github.com/creack/pty"
	"strings"
	"testing"
)

func TestCanonicalPromptDoesNotSubmitTruncatedData(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if got := submit(master, strings.Repeat("x", 4096)); got != "too_long" {
		t.Fatal(got)
	}
	if got := submit(master, "next"); got != "submitted" {
		t.Fatal(got)
	}
	buf := make([]byte, 4096)
	n, err := slave.Read(buf)
	if err != nil || string(buf[:n]) != "next\n" {
		t.Fatalf("read %d bytes: %v", n, err)
	}
}
