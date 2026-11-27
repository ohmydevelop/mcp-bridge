package terminal

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Child applications negotiate bracketed paste through terminal output.
type pasteMode struct {
	enabled atomic.Bool
	tail    []byte
}

func (m *pasteMode) Write(chunk []byte) (int, error) {
	data := append(m.tail, chunk...)
	on := bytes.LastIndex(data, []byte("\x1b[?2004h"))
	off := bytes.LastIndex(data, []byte("\x1b[?2004l"))
	if on >= 0 || off >= 0 {
		m.enabled.Store(on > off)
	}
	m.tail = append(m.tail[:0], data[max(0, len(data)-7):]...)
	return len(chunk), nil
}

// Run keeps the master only for the lifetime of this child. Never fall back to a shell.
func Run(socket, mode string, argv []string) error {
	if len(argv) == 0 || (mode != "inbox" && mode != "auto") {
		return errors.New("usage: mcp-bridge run [--delivery=inbox|auto] -- PROGRAM [ARG...]")
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("run requires an interactive terminal")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	if _, err := request(socket, token, "register", ""); err != nil {
		return err
	}
	defer request(socket, token, "close", "")
	state, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, state)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), Env+"="+token)
	size, err := pty.GetsizeFull(os.Stdin)
	if err != nil {
		return err
	}
	master, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return err
	}
	defer master.Close()
	var paste pasteMode
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	go io.Copy(io.MultiWriter(&paste, os.Stdout), master)
	var write sync.Mutex
	keys := make(chan bool, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, e := os.Stdin.Read(buf)
			if e != nil {
				return
			}
			write.Lock()
			data := buf[:n]
			if mode == "inbox" {
				for _, ch := range data {
					if ch == 29 {
						select {
						case keys <- true:
						default:
						}
					}
				}
				data = bytes.ReplaceAll(data, []byte{29}, nil)
			}
			if _, err := master.Write(data); err != nil {
				write.Unlock()
				return
			}
			write.Unlock()
		}
	}()
	fmt.Fprintf(os.Stderr, "\r\n[bridge %s, %s; inbox: Ctrl+] to preview, again to submit]\r\n", ID(token), mode)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	var preview, notice, ackID, ack string
	for {
		pressed := false
		select {
		case err := <-done:
			return err
		case sig := <-stop:
			_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
			_ = master.Close()
			select {
			case err := <-done:
				return err
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				return <-done
			}
		case <-resize:
			_ = pty.InheritSize(os.Stdin, master)
			continue
		case <-keys:
			pressed = true
		case <-tick.C:
		}
		if ackID != "" {
			if _, err = request(socket, token, ack, ackID); err != nil {
				continue
			}
			ackID = ""
		}
		r, e := request(socket, token, "peek", "")
		if e != nil {
			_, _ = request(socket, token, "register", "")
			continue
		}
		if r.ID == "" {
			preview = ""
			notice = ""
			continue
		}
		if mode == "inbox" {
			if !pressed {
				if notice != r.ID {
					fmt.Fprintf(os.Stderr, "\r\n[bridge: %d pending; Ctrl+] to preview]\r\n", r.Count)
					notice = r.ID
				}
				continue
			}
			if preview != r.ID {
				fmt.Fprintf(os.Stderr, "\r\n[bridge preview] %s\r\n[Ctrl+] again when ready to submit]\r\n", r.Prompt)
				preview = r.ID
				continue
			}
		}

		r, e = request(socket, token, "claim", r.ID)
		if e != nil || r.ID == "" {
			continue
		}
		// A single writer prevents keyboard bytes from interleaving with this submission.
		write.Lock()
		input := r.Prompt
		if paste.enabled.Load() {
			input = "\x1b[200~" + input + "\x1b[201~"
		}
		ack = submit(master, input)
		write.Unlock()
		ackID = r.ID
		if _, err = request(socket, token, ack, ackID); err == nil {
			ackID = ""
		}
		preview = ""
	}
}

// Linux canonical mode accepts 4095 input bytes plus a line delimiter.
func submit(master *os.File, input string) string {
	settings, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
	if err != nil {
		return "unknown"
	}
	if settings.Lflag&unix.ICANON != 0 && len(input) > 4095 {
		return "too_long"
	}
	if _, err = io.WriteString(master, input); err != nil {
		return "unknown"
	}
	time.Sleep(120 * time.Millisecond)
	if _, err = io.WriteString(master, "\r"); err != nil {
		return "unknown"
	}
	return "submitted"
}
