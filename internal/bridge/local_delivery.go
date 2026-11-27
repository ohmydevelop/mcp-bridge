package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"newmcp/mcp-bridge/internal/storage"
	"newmcp/mcp-bridge/internal/terminal"
)

type terminalBinding struct{}
type cliPreset struct {
	ID      string   `json:"id"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd"`
	Format  string   `json:"input_format"`
	Grants  []string `json:"grants,omitempty"`
}

func (d Destination) kind() string {
	if d.Type == "" {
		return "webhook"
	}
	return d.Type
}
func (b *Bridge) loadLocalDestinations() error {
	var list []cliPreset
	raw, err := os.ReadFile(filepath.Join(b.dir, "cli-destinations.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal(raw, &list); err != nil {
			return err
		}
	}
	b.presets = map[string]cliPreset{}
	tx, err := b.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored, err := storage.ListTx[Destination](b.store, tx, "destination")
	if err != nil {
		return err
	}
	existing := map[string]Destination{}
	for _, d := range stored {
		existing[d.ID] = d
	}
	for _, c := range list {
		if !identifier.MatchString(c.ID) || !filepath.IsAbs(c.Command) || !filepath.IsAbs(c.Cwd) || (c.Format != "json" && c.Format != "prompt") {
			return fmt.Errorf("invalid cli preset %q", c.ID)
		}
		if _, ok := b.presets[c.ID]; ok {
			return fmt.Errorf("duplicate preset %s", c.ID)
		}
		if _, err = exec.LookPath(c.Command); err != nil {
			return err
		}
		dir, err := os.Stat(c.Cwd)
		if err != nil || !dir.IsDir() {
			return fmt.Errorf("invalid preset cwd %s", c.ID)
		}
		d := Destination{ID: c.ID, Type: "cli_process", Owner: localIdentity, Grants: c.Grants}
		prev, ok := existing[c.ID]
		if ok && prev.kind() != d.Type {
			return fmt.Errorf("destination %s is already in use", c.ID)
		}
		if !equal(d, prev) {
			rev := 0
			if ok {
				if err = tx.QueryRow("SELECT revision FROM objects WHERE kind='destination' AND id=?", c.ID).Scan(&rev); err != nil {
					return err
				}
			}
			if err = b.store.PutTx(tx, "system", "destination", c.ID, d, rev); err != nil {
				return err
			}
		}
		b.presets[c.ID] = c
	}
	// Once external submission starts a restart cannot establish its outcome.
	if _, err = tx.Exec("UPDATE deliveries SET status='unknown',error='interrupted during submission' WHERE status='submitting'"); err != nil {
		return err
	}
	return tx.Commit()
}
func (b *Bridge) destinationStatus(p Principal, revisions map[string]int) ([]Object, error) {
	ds, err := storage.List[Destination](b.store, "destination")
	if err != nil {
		return nil, err
	}
	out := []Object{}
	for _, d := range ds {
		if !canUse(p.Name, d.Owner, d.Grants) {
			delete(revisions, d.ID)
			continue
		}
		row := Object{"id": d.ID, "type": d.kind(), "revision": revisions[d.ID]}
		switch d.kind() {
		case "terminal_session":
			row["online"] = b.terminalOnline(d.ID)
			delete(revisions, d.ID)
		case "cli_process":
			_, row["online"] = b.presets[d.ID]
			delete(revisions, d.ID)
		default:
			row["online"] = nil
		}
		out = append(out, row)
	}
	return out, nil
}
func (b *Bridge) terminalOnline(id string) bool {
	seen, ok := b.terminals.Load(id)
	return ok && time.Since(seen.(time.Time)) < 5*time.Second
}

type localJob struct {
	ID, Flow string
	Payload  json.RawMessage
}

func (b *Bridge) nextLocal(dest, kind string) (localJob, error) {
	var j localJob
	var raw []byte
	err := b.store.DB.QueryRow("SELECT d.id,d.flow,d.data FROM deliveries d JOIN local_deliveries l ON l.id=d.id WHERE d.destination=? AND l.kind=? AND d.status='pending' AND d.next<=unixepoch() ORDER BY d.rowid LIMIT 1", dest, kind).Scan(&j.ID, &j.Flow, &raw)
	if err != nil {
		return j, err
	}
	var f Flow
	var d Destination
	if _, err = b.store.Get("flow", j.Flow, &f); err != nil {
		return j, err
	}
	if _, err = b.store.Get("destination", dest, &d); err != nil {
		return j, err
	}
	if f.Disabled || !canUse(f.Owner, d.Owner, d.Grants) || (f.Schedule == nil && !b.visible(Principal{f.Owner}, f.Source)) {
		_, err = b.store.DB.Exec("UPDATE deliveries SET status='suppressed' WHERE id=? AND status='pending'", j.ID)
		if err == nil {
			err = sql.ErrNoRows
		}
		return j, err
	}
	err = b.store.Decode("delivery", j.ID, raw, &j.Payload)
	return j, err
}
func renderPrompt(raw json.RawMessage) (string, error) {
	var body Object
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", err
	}
	var lines []string
	for _, k := range []string{"instruction", "skill_ref"} {
		if v, ok := body[k].(string); ok && v != "" {
			lines = append(lines, k+": "+v)
		}
		delete(body, k)
	}
	rest, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return strings.Join(append(lines, "Notification: "+string(rest)), "\n"), nil
}
func terminalText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
func (b *Bridge) claim(id string) error {
	r, e := b.store.DB.Exec("UPDATE deliveries SET status='submitting',attempt=attempt+1,next=unixepoch() WHERE id=? AND status='pending'", id)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return e
}

func (b *Bridge) terminalRequest(req terminal.Request) (terminal.Reply, error) {
	var reply terminal.Reply
	if _, err := hex.DecodeString(req.Token); len(req.Token) != 64 || err != nil {
		return reply, errors.New("invalid terminal token")
	}
	id := terminal.ID(req.Token)
	switch req.Action {
	case "register":
		b.modify.Lock()
		defer b.modify.Unlock()
		var d Destination
		_, err := b.store.Get("destination", id, &d)
		if errors.Is(err, sql.ErrNoRows) {
			err = b.store.Put(localIdentity, "destination", id, Destination{ID: id, Type: "terminal_session", Owner: localIdentity}, 0)
		} else if err == nil && d.Type != "terminal_session" {
			err = errors.New("destination collision")
		}
		if err == nil {
			b.terminals.Store(id, time.Now())
		}
		return reply, err
	case "close":
		b.terminals.Delete(id)
		_, err := b.store.DB.Exec("UPDATE deliveries SET status='unknown' WHERE destination=? AND status='submitting'", id)
		return reply, err
	case "submitted", "unknown", "too_long":
		status, reason := req.Action, ""
		if req.Action == "too_long" {
			status, reason = "failed", "prompt exceeds terminal canonical line limit"
		}
		_, err := b.store.DB.Exec("UPDATE deliveries SET status=?,error=? WHERE destination=? AND id=? AND status IN ('submitting','unknown')", status, reason, id, req.ID)
		return reply, err
	}
	if _, ok := b.terminals.Load(id); !ok {
		return reply, errors.New("register terminal")
	}
	b.terminals.Store(id, time.Now())
	if req.Action != "peek" && req.Action != "claim" {
		return reply, errors.New("invalid terminal operation")
	}
	b.modify.Lock()
	defer b.modify.Unlock()
	j, err := b.nextLocal(id, "terminal_session")
	if errors.Is(err, sql.ErrNoRows) {
		return reply, nil
	}
	if err != nil {
		return reply, err
	}
	if req.Action == "claim" {
		if j.ID != req.ID {
			return reply, errors.New("queue changed")
		}
		if err = b.claim(j.ID); err != nil {
			return reply, err
		}
	}
	reply.ID = j.ID
	prompt, err := renderPrompt(j.Payload)
	reply.Prompt = terminalText(prompt)
	if err == nil {
		err = b.store.DB.QueryRow("SELECT count(*) FROM deliveries WHERE destination=? AND status='pending'", id).Scan(&reply.Count)
	}
	return reply, err
}

type boundedBuffer struct{ buf bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := 8192 - b.buf.Len(); room > 0 {
		_, _ = b.buf.Write(p[:min(n, room)])
	}
	return n, nil
}

func (b *Bridge) processCLI(ctx context.Context, c cliPreset) error {
	b.modify.Lock()
	j, err := b.nextLocal(c.ID, "cli_process")
	if err == nil {
		err = b.claim(j.ID)
	}
	b.modify.Unlock()
	if err != nil {
		return err
	}
	input := string(j.Payload)
	if c.Format == "prompt" {
		input, err = renderPrompt(j.Payload)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Command, c.Args...)
	cmd.Dir = c.Cwd
	cmd.Stdin = strings.NewReader(input + "\n")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stdout, stderr boundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	state := "completed"
	reason := ""
	if err != nil {
		state = "failed"
		reason = err.Error()
	}
	if errors.Is(ctx.Err(), context.Canceled) && cmd.Process != nil {
		state = "unknown"
		reason = "daemon stopped during execution"
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		state = "failed"
		reason = "execution timed out"
	}
	result := Object{"stdout": stdout.buf.String(), "stderr": stderr.buf.String()}
	if cmd.ProcessState != nil {
		result["exit_code"] = cmd.ProcessState.ExitCode()
	}
	blob, err := b.store.Seal("delivery-result", j.ID, result)
	if err != nil {
		return err
	}
	tx, err := b.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE local_deliveries SET result=? WHERE id=?", blob, j.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE deliveries SET status=?,error=? WHERE id=? AND status='submitting'", state, reason, j.ID); err != nil {
		return err
	}
	return tx.Commit()
}
