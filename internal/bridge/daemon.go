package bridge

import (
	"bufio"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"newmcp/mcp-bridge/internal/terminal"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"
)

// Serve runs the bridge daemon using the absolute state directory.
func Serve(abs, listen string, registerApp func(*mcp.Server)) error {
	var e error
	socket := filepath.Join(abs, "bridge.sock")
	syscall.Umask(0077)
	if e = os.MkdirAll(abs, 0700); e != nil {
		return e
	}
	info, e := os.Stat(abs)
	if e != nil || info.Mode().Perm()&0077 != 0 {
		return errors.New("state directory must be private (0700)")
	}
	lock, e := os.OpenFile(filepath.Join(abs, "bridge.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		return errors.New("state directory already in use")
	}
	key, e := hex.DecodeString(os.Getenv("BRIDGE_MASTER_KEY"))
	if e != nil || len(key) != 32 {
		return errors.New("BRIDGE_MASTER_KEY requires 64 hex characters")
	}
	tokens := map[string]string{}
	if raw := os.Getenv("BRIDGE_TOKENS"); raw != "" {
		if e = json.Unmarshal([]byte(raw), &tokens); e != nil {
			return errors.New("invalid BRIDGE_TOKENS")
		}
	}
	known := map[string]bool{}
	for name, t := range tokens {
		if !identifier.MatchString(name) || len(t) < 32 || known[t] {
			return errors.New("invalid principal or duplicate/short token")
		}
		known[t] = true
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	b, e := openBridge(ctx, abs, key)
	if e != nil {
		return e
	}
	defer b.store.DB.Close()
	b.registerApp = registerApp
	if raw := os.Getenv("BRIDGE_PROFILES"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &b.profiles); err != nil {
			return errors.New("invalid BRIDGE_PROFILES")
		}
		for name, profile := range b.profiles {
			if _, ok := tokens[name]; !ok && name != localIdentity {
				return fmt.Errorf("unknown profile %s", name)
			}
			if profile.Producer != "" && (name == localIdentity || !identifier.MatchString(profile.Producer)) {
				return errors.New("invalid producer profile")
			}
		}
	}
	if e = b.loadLocalDestinations(); e != nil {
		return e
	}
	if e = b.migrateTriggers(); e != nil {
		return e
	}
	if e = b.migrateBindings(); e != nil {
		return e
	}
	for _, s := range b.sources {
		_, e = b.reconcile(ctx, s)
		if e != nil {
			log.Printf("source %s could not initialize", s.ID)
		}
	}
	if e = os.Remove(socket); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	ln, e := net.Listen("unix", socket)
	if e != nil {
		return e
	}
	defer ln.Close()
	var jobs sync.WaitGroup
	jobs.Go(func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			jobs.Go(func() {
				defer conn.Close()
				b.localConnection(ctx, conn)
			})
		}
	})
	for _, preset := range b.presets {
		jobs.Go(func() {
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := b.processCLI(ctx, preset); err != nil && !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
						log.Print("cli delivery: ", err)
					}
				}
			}
		})
	}
	handlers := map[string]http.Handler{}
	for name := range tokens {
		p := Principal{name}
		s := b.server(p)
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, MaxRequestBodyBytes: 8 << 20})
		handlers[name] = handler
		if b.profiles[name].Producer == "" {
			handlers[name] = b.httpMCP(p, handler)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /callback/{flow}", b.receiveHook)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		for name, k := range tokens {
			if subtle.ConstantTimeCompare([]byte(token), []byte("Bearer "+k)) == 1 {
				w.Header().Set("Cache-Control", "private, no-store")
				handlers[name].ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", 401)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if e := b.store.DB.Ping(); e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	srv := http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 32768}
	jobs.Go(func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		var checked time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if minute := now.Truncate(time.Minute); minute != checked {
					if _, err := b.store.DB.Exec("UPDATE deliveries SET status='unknown',error='submission receipt expired' WHERE status='submitting' AND next < unixepoch()-30 AND id IN (SELECT id FROM local_deliveries WHERE kind='terminal_session')"); err != nil && ctx.Err() == nil {
						log.Print("terminal receipts: ", err)
					}
					if err := b.scheduleDue(now); err != nil && ctx.Err() == nil {
						log.Print("scheduler: ", err)
					} else {
						checked = minute
					}
				}
				if err := b.processRun(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
					log.Print("trigger worker: ", err)
				}
			}
		}
	})
	jobs.Go(func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := b.reviewCandidates(ctx); err != nil && ctx.Err() == nil {
					log.Print("candidate review: ", err)
				}
			}
		}
	})
	jobs.Go(func() {
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e := b.deliver(ctx); e != nil && !errors.Is(e, sql.ErrNoRows) && ctx.Err() == nil {
					log.Print("delivery worker: ", e)
				}
			}
		}
	})
	jobs.Go(func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e := b.enqueueSubscribed(ctx); e != nil && ctx.Err() == nil {
					log.Print("subscription worker: ", e)
				}
				if e := b.pollUpstreams(ctx); e != nil && ctx.Err() == nil {
					log.Print("poll worker: ", e)
				}
			}
		}
	})
	jobs.Go(func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.syncSources(ctx)
			}
		}
	})
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case e = <-done:
	}
	stop()
	ln.Close()
	srv.Close()
	jobs.Wait()
	b.streamWG.Wait()
	for _, r := range b.live {
		r.session.Close()
	}
	return e
}

func (b *Bridge) localConnection(ctx context.Context, c net.Conn) {
	done := context.AfterFunc(ctx, func() { c.Close() })
	defer done()
	r := bufio.NewReader(c)
	first, err := r.Peek(1)
	if err != nil {
		return
	}
	switch first[0] {
	case 'T':
		r.ReadByte()
		var req terminal.Request
		if err = json.NewDecoder(io.LimitReader(r, 2048)).Decode(&req); err != nil {
			return
		}
		reply, err := b.terminalRequest(req)
		if err != nil {
			reply.Error = err.Error()
		}
		json.NewEncoder(c).Encode(reply)
		return
	case 'B':
		r.ReadByte()
		line, err := r.ReadSlice('\n')
		if err != nil || len(line) != 65 {
			return
		}
		id := terminal.ID(strings.TrimSuffix(string(line), "\n"))
		if !b.terminalOnline(id) {
			return
		}
		ctx = context.WithValue(ctx, terminalBinding{}, id)
	}
	reader := struct {
		io.Reader
		io.Closer
	}{r, c}
	_ = b.server(Principal{localIdentity}).Run(ctx, eventTransport{&mcp.IOTransport{Reader: reader, Writer: c}, b, Principal{localIdentity}})
}
