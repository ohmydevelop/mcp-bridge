package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"newmcp/mcp-bridge/internal/localipc"
)

type localHealth struct {
	Bridge bool   `json:"bridge"`
	Python string `json:"python,omitempty"`
	Error  string `json:"error,omitempty"`
}

// CheckLocal probes the running service's Python environment, including isolation.
func CheckLocal(dir string, w io.Writer) error {
	c, err := localipc.Dial(dir)
	if err != nil {
		return fmt.Errorf("local Bridge connection: %w", err)
	}
	defer c.Close()
	if err = c.SetDeadline(time.Now().Add(time.Minute)); err != nil {
		return err
	}
	if _, err = io.WriteString(c, "D"); err != nil {
		return err
	}
	var health localHealth
	if err = json.NewDecoder(io.LimitReader(c, 1<<16)).Decode(&health); err != nil {
		return fmt.Errorf("local Bridge diagnostic: %w", err)
	}
	if health.Bridge {
		fmt.Fprintln(w, "Bridge: online (local IPC and database)")
	}
	if health.Error != "" {
		return errors.New(health.Error)
	}
	fmt.Fprintln(w, "Isolated native Python:", health.Python)
	return nil
}
