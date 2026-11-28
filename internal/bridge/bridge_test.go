package bridge

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"newmcp/mcp-bridge/internal/plugin"
)

func testBridge(t *testing.T) *Bridge {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b, e := openBridge(ctx, t.TempDir(), bytes.Repeat([]byte{42}, 32))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cancel(); b.streamWG.Wait(); b.store.DB.Close() })
	if err := b.migrateTriggers(); err != nil {
		t.Fatal(err)
	}
	return b
}
func clientFor(t *testing.T, b *Bridge, p Principal) *mcp.ClientSession {
	t.Helper()
	server, client := mcp.NewInMemoryTransports()
	ss, e := b.server(p).Connect(t.Context(), server, nil)
	if e != nil {
		t.Fatal(e)
	}
	c := newClient()
	cs, e := c.Connect(t.Context(), client, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cs.Close(); ss.Close() })
	return cs
}
func TestModernSkillsAndAccess(t *testing.T) {
	b := testBridge(t)
	cs := clientFor(t, b, Principal{"reader"})
	if cs.InitializeResult().ProtocolVersion != "2026-07-28" {
		t.Fatal(cs.InitializeResult())
	}
	skills, e := external(t.Context(), cs, skillsExt, "skills/list", &extParams{})
	if e != nil {
		t.Fatal(e)
	}
	if len(skills["skills"].([]any)) != 2 {
		t.Fatal(skills)
	}
	uri := "skill://bridge.builtin/setup-webhook/SKILL.md"
	sk, e := external(t.Context(), cs, skillsExt, "skills/get", &extParams{URI: uri})
	if e != nil {
		t.Fatal(e)
	}
	if sk["skill"].(Object)["uri"] != uri {
		t.Fatal(sk)
	}
	res, e := cs.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri})
	if e != nil || len(res.Contents) != 1 {
		t.Fatal(e)
	}
	for _, e := range cs.Tools(t.Context(), nil) {
		if e != nil {
			t.Fatal(e)
		}
	}
}
func TestWebhookSubscription(t *testing.T) {
	b := testBridge(t)
	p := Principal{"admin"}
	ctx := t.Context()
	cs := clientFor(t, b, p)
	var body []byte
	var sig, id, ts string
	recv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		var control struct{ Challenge string }
		_ = json.Unmarshal(body, &control)
		if control.Challenge != "" {
			json.NewEncoder(w).Encode(control)
			return
		}
		sig = r.Header.Get("webhook-signature")
		id = r.Header.Get("webhook-id")
		ts = r.Header.Get("webhook-timestamp")
		w.WriteHeader(204)
	}))
	defer recv.Close()
	t.Setenv("BRIDGE_WEBHOOK_ORIGINS", recv.URL)
	original := http.DefaultTransport
	http.DefaultTransport = recv.Client().Transport
	defer func() { http.DefaultTransport = original }()
	rawKey := bytes.Repeat([]byte{7}, 32)
	params := &extParams{Name: "bridge.builtin__source.added", Delivery: &callback{"webhook", recv.URL, "whsec_" + base64.StdEncoding.EncodeToString(rawKey)}}
	result, e := external(ctx, cs, eventsExt, "events/subscribe", params)
	if e != nil || result["id"] == nil {
		t.Fatal(result, e)
	}
	again, e := external(ctx, cs, eventsExt, "events/subscribe", params)
	if e != nil || again["id"] != result["id"] {
		t.Fatal(again, e)
	}
	if e = b.appendEvents(ctx, "bridge.builtin", []Occurrence{{ID: "evt", Name: "source.added", Data: Object{"source_id": "qa"}}}, "", nil); e != nil {
		t.Fatal(e)
	}
	if e = b.enqueueSubscribed(ctx); e != nil {
		t.Fatal(e)
	}
	if e = b.deliver(ctx); e != nil {
		t.Fatal(e)
	}
	if sig != signature(rawKey, id, ts, body) || !bytes.Contains(body, []byte("bridge.builtin__source.added")) {
		t.Fatalf("invalid subscription delivery: %q", body)
	}
	if _, e = external(ctx, cs, eventsExt, "events/unsubscribe", params); e != nil {
		t.Fatal(e)
	}
}
func TestSnapshotRollbackAndMCPB(t *testing.T) {
	b := testBridge(t)
	p := Principal{"admin"}
	dir := t.TempDir()
	manifest := Object{"manifest_version": "0.4", "name": "qa", "version": "1.0.0", "description": "qa", "author": Object{"name": "QA"}, "server": Object{"type": "binary", "entry_point": "qa", "mcp_config": Object{"command": "qa", "args": []string{"--endpoint", "${user_config.endpoint}"}, "env": Object{"TOKEN": "${user_config.token}"}}}, "user_config": Object{"endpoint": Object{"type": "string", "title": "Endpoint", "description": "QA", "required": true}, "token": Object{"type": "string", "title": "Key", "description": "QA token", "sensitive": true}}}
	raw, _ := json.Marshal(manifest)
	if e := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	stage := t.TempDir()
	id, e := plugin.Snapshot(dir, stage)
	if e != nil {
		t.Fatal(e)
	}
	pkgRoot, _ := b.packageDir(id)
	if e = os.MkdirAll(filepath.Dir(pkgRoot), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(stage, pkgRoot); e != nil {
		t.Fatal(e)
	}
	t.Setenv("BRIDGE_CRED_QA", "top-secret")
	s := Source{ID: "qa", Package: id, Server: "default", Settings: Object{"endpoint": "QA URL with spaces", "token": Object{"credential": "QA"}}}
	cfg, e := b.resolve(s)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Args[1] != "QA URL with spaces" || cfg.Env["TOKEN"] != "top-secret" {
		t.Fatal("invalid settings substitution")
	}
	s.Disabled = true
	if _, e = b.saveSource(t.Context(), p, s, 0); e != nil {
		t.Fatal(e)
	}
	s.Disabled = false
	if _, e = b.saveSource(t.Context(), p, s, 1); e == nil {
		t.Fatal("expected failed probe")
	}
	var saved Source
	rev, e := b.store.Get("source", "qa", &saved)
	if e != nil || rev != 1 || !saved.Disabled {
		t.Fatal("rollback failed")
	}
	var cipher []byte
	_ = b.store.DB.QueryRow("SELECT data FROM objects WHERE kind='source'").Scan(&cipher)
	if bytes.Contains(cipher, []byte("endpoint")) {
		t.Fatal("plaintext settings")
	}
}
func TestArchiveAndSkillIntegrity(t *testing.T) {
	var ar bytes.Buffer
	w := zip.NewWriter(&ar)
	h := &zip.FileHeader{Name: "escape"}
	h.SetMode(os.ModeSymlink | 0777)
	entry, _ := w.CreateHeader(h)
	io.WriteString(entry, "/etc/passwd")
	w.Close()
	path := filepath.Join(t.TempDir(), "p.mcpb")
	os.WriteFile(path, ar.Bytes(), 0600)
	if _, e := plugin.Snapshot(path, t.TempDir()); e == nil {
		t.Fatal("symlink accepted")
	}
	sk, e := localSkill("builtin", "setup-webhook", map[string][]byte{"SKILL.md": []byte(guide)})
	if e != nil {
		t.Fatal(e)
	}
	sk.bytes[sk.URI] = []byte("tampered")
	if _, e = sk.read(t.Context(), sk.URI); e == nil {
		t.Fatal("integrity failure ignored")
	}
}
func TestStreamCursorAndCancellation(t *testing.T) {
	b := testBridge(t)
	p := Principal{"admin"}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var methods []string
	e := b.stream(ctx, p, streamRequest{ID: 1, Method: "events/stream", Params: extParams{ParamsBase: mcp.ParamsBase{Meta: mcp.Meta{mcp.MetaKeyProtocolVersion: "2026-07-28"}}, Name: "bridge.builtin__source.added"}}, func(o Object) error {
		method, _ := o["method"].(string)
		methods = append(methods, method)
		switch method {
		case "notifications/events/active":
			return b.appendEvents(ctx, "bridge.builtin", []Occurrence{{ID: "stream", Name: "source.added", Data: Object{"source_id": "qa"}}}, "", nil)
		case "notifications/events/event":
			cancel()
		}
		return nil
	})
	if e != nil || strings.Join(methods, ",") != "notifications/events/active,notifications/events/event," {
		t.Fatal(e, methods)
	}
}

func TestHTTPBridgeEventRouting(t *testing.T) {
	ctx := t.Context()
	admin := Principal{"admin"}
	upstream := testBridge(t)
	server := upstream.server(admin)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	endpoint := httptest.NewServer(upstream.httpMCP(admin, transport))
	defer endpoint.Close()
	downstream := testBridge(t)
	if _, err := downstream.saveSource(ctx, admin, Source{ID: "peer", Type: "streamable-http", URL: endpoint.URL}, 0); err != nil {
		t.Fatal(err)
	}
	defer downstream.live["peer"].session.Close()
	for _, mode := range []string{"poll", "push"} {
		t.Run(mode, func(t *testing.T) {
			flow := Flow{ID: mode, Source: "peer", Name: "bridge.builtin__source.added", Owner: "admin", Mode: mode}
			if err := downstream.store.Put(admin.Name, "flow", mode, flow, 0); err != nil {
				t.Fatal(err)
			}
			if mode == "push" {
				sub, end := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { done <- downstream.streamFlow(sub, admin, flow) }()
				defer func() {
					end()
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("stream stuck")
					}
				}()
			} else if err := downstream.drainFlow(ctx, flow, admin); err != nil {
				t.Fatal(err)
			}
			until := func(check func() bool) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for !check() {
					if time.Now().After(deadline) {
						t.Fatal("deadline exceeded")
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			until(func() bool { var cur any; _, err := downstream.store.Get("cursor", mode, &cur); return err == nil })
			if err := upstream.appendEvents(ctx, "bridge.builtin", []Occurrence{{ID: mode, Name: "source.added", Data: Object{"source_id": "qa"}}}, "", nil); err != nil {
				t.Fatal(err)
			}
			if mode == "poll" {
				downstream.pollSchedule.Delete(mode)
				if err := downstream.drainFlow(ctx, flow, admin); err != nil {
					t.Fatal(err)
				}
			}
			until(func() bool {
				var count int
				err := downstream.store.DB.QueryRow("SELECT COUNT(*) FROM events WHERE source='peer' AND eventid=?", mode).Scan(&count)
				return err == nil && count == 1
			})
		})
	}
}
