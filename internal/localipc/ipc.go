// Package localipc provides the current user's private Bridge control channel.
package localipc

import (
	"io"
	"net"
)

// Conn also permits stdin EOF without discarding outstanding replies.
type Conn interface {
	net.Conn
	CloseWrite() error
}

func Lock(dir string) (io.Closer, error) {
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	return lockDir(dir)
}
