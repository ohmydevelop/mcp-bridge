package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"newmcp/mcp-bridge/internal/platform"
	"newmcp/mcp-bridge/internal/storage"
	"newmcp/mcp-bridge/internal/terminal"
)

type terminalBinding struct{}
type localConnectionKey struct{}

func (d Destination) validateCLI() error {
	if !filepath.IsAbs(d.Command) || !filepath.IsAbs(d.Cwd) || (d.Format != "json" && d.Format != "prompt") {
		return fmt.Errorf("invalid CLI configuration %q: require absolute command and cwd and input_format json or prompt", d.ID)
	}
	if _, err := exec.LookPath(d.Command); err != nil {
		return err
	}
	dir, err := os.Stat(d.Cwd)
	if err != nil || !dir.IsDir() {
		return fmt.Errorf("invalid CLI cwd for %s", d.ID)
	}
	return nil
}
func (d Destination) kind() string {
	if d.Type == "" {
		return "webhook"
	}
	return d.Type
}
func (b *Bridge) loadLocalDestinations() error {
	var list []Destination
	raw, err := os.ReadFile(filepath.Join(b.dir, "cli-destinations.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal(raw, &list); err != nil {
			return err
		}
	}
	b.presets = map[string]Destination{}
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
		if !identifier.MatchString(c.ID) {
			return fmt.Errorf("invalid cli preset %q", c.ID)
		}
		if _, ok := b.presets[c.ID]; ok {
			return fmt.Errorf("duplicate preset %s", c.ID)
		}
		b.presets[c.ID] = c
		if existing[c.ID].kind() == "cli_process" && existing[c.ID].Command != "" {
			continue
		}
		if err := c.validateCLI(); err != nil {
			return err
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
	refs := map[string]int{}
	flows, err := storage.List[Flow](b.store, "flow")
	if err != nil {
		return nil, err
	}
	for _, f := range flows {
		refs[f.Destination]++
	}
	out := []Object{}
	for _, d := range ds {
		if !canUse(p.Name, d.Owner, d.Grants) {
			delete(revisions, d.ID)
			continue
		}
		row := Object{"id": d.ID, "type": d.kind(), "revision": revisions[d.ID], "trigger_count": refs[d.ID]}
		switch d.kind() {
		case "terminal_session":
			row["online"] = b.terminalOnline(d.ID)
			delete(revisions, d.ID)
		case "cli_process":
			_, fromFile := b.presets[d.ID]
			row["online"] = d.Command != "" || fromFile
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
	ID, Flow    string
	Payload     json.RawMessage
	Destination Destination
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
	j.Destination = d
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

func (b *Bridge) dispatchCLI(ctx context.Context, busy *sync.Map, jobs *sync.WaitGroup) error {
	rows, err := b.store.DB.QueryContext(ctx, `SELECT DISTINCT d.destination FROM deliveries d
        JOIN local_deliveries l ON l.id=d.id
        WHERE d.status='pending' AND d.next<=unixepoch() AND l.kind='cli_process'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if _, running := busy.LoadOrStore(id, true); running {
			continue
		}
		jobs.Go(func() {
			defer busy.Delete(id)
			if err := b.processCLI(ctx, id); err != nil && !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
				log.Print("cli delivery: ", err)
			}
		})
	}
	return rows.Err()
}

func (b *Bridge) processCLI(ctx context.Context, id string) error {
	b.modify.Lock()
	j, err := b.nextLocal(id, "cli_process")
	c := j.Destination
	if err == nil && c.Command == "" {
		var ok bool
		if c, ok = b.presets[id]; !ok {
			err = sql.ErrNoRows
		}
	}
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
	cmd, err := platform.CLICommand(ctx, c.Command, c.Args...)
	var stdout, stderr boundedBuffer
	if err == nil {
		cmd.Dir = c.Cwd
		cmd.Stdin = strings.NewReader(input + "\n")
		cmd.Env = platform.ChildEnv(true)
		cmd.WaitDelay = time.Second
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err = cmd.Run()
	}
	state := "completed"
	reason := ""
	if err != nil {
		state = "failed"
		reason = err.Error()
	}
	if errors.Is(ctx.Err(), context.Canceled) && cmd != nil && cmd.Process != nil {
		state = "unknown"
		reason = "daemon stopped during execution"
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		state = "failed"
		reason = "execution timed out"
	}
	result := Object{"stdout": stdout.buf.String(), "stderr": stderr.buf.String()}
	if cmd != nil && cmd.ProcessState != nil {
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
