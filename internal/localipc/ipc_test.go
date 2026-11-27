package localipc

import (
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestLockAndHalfClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if duplicate, err := Lock(dir); err == nil {
		duplicate.Close()
		t.Fatal("second daemon acquired the same directory")
	}
	listener, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		payload, err := io.ReadAll(c)
		if err == nil {
			_, err = c.Write(append([]byte("reply: "), payload...))
		}
		done <- err
	}()
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(c)
	if err != nil || string(reply) != "reply: ping" {
		t.Fatalf("reply %q: %v", reply, err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
