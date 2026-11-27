//go:build !linux

package terminal

import "errors"

const Supported = false

func Run(dir, delivery string, args []string) error {
	return errors.New("interactive terminal sessions require Linux; use a cli_process destination on this platform")
}
