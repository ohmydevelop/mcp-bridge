// Package terminal connects a local PTY launcher to the Bridge's private socket.
package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

const Env = "MCP_BRIDGE_TERMINAL_TOKEN"

type Request struct {
	Token  string
	Action string
	ID     string
}
type Reply struct {
	Error  string
	ID     string
	Prompt string
	Count  int
}

func ID(token string) string {
	s := sha256.Sum256([]byte(token))
	return "term-" + hex.EncodeToString(s[:16])
}
func request(socket, token, action, id string) (Reply, error) {
	var out Reply
	c, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return out, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = c.Write([]byte("T")); err == nil {
		err = json.NewEncoder(c).Encode(Request{token, action, id})
	}
	if err == nil {
		err = json.NewDecoder(c).Decode(&out)
	}
	if err == nil && out.Error != "" {
		err = fmt.Errorf("terminal service: %s", out.Error)
	}
	return out, err
}
