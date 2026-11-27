package bridge

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"newmcp/mcp-bridge/internal/storage"
)

type triggerWhen struct {
	Type   string `json:"type"`
	Source string `json:"source_id,omitempty"`
	Event  string `json:"event_name,omitempty"`
	Mode   string `json:"ingestion_mode,omitempty"`
	Filter Object `json:"filter,omitempty"`
	Cron   string `json:"cron,omitempty"`
	Zone   string `json:"timezone,omitempty"`
}
type toolVersion struct {
	ID       string `json:"tool_id"`
	Revision int    `json:"revision,omitempty"`
}

func (b *Bridge) readVersion(ref toolVersion, p Principal) (Binding, int, error) {
	var d Binding
	id, ok := strings.CutPrefix(ref.ID, "bridge.builtin/")
	if !ok || id == "" || ref.Revision < 0 {
		return d, 0, errors.New("expected saved tool_id")
	}
	var err error
	rev := ref.Revision
	if rev == 0 {
		rev, err = b.store.Get("binding", id, &d)
	} else {
		var raw []byte
		err = b.store.DB.QueryRow("SELECT data FROM binding_versions WHERE id=? AND revision=?", id, rev).Scan(&raw)
		if err == nil {
			err = b.store.Decode("binding", id, raw, &d)
		}
	}
	if err != nil {
		return d, 0, err
	}
	b.mu.RLock()
	ok = b.bindingAllowed(p, d)
	b.mu.RUnlock()
	if !ok {
		return d, 0, errors.New("tool unavailable")
	}
	return d, rev, nil
}

func (w triggerWhen) schedule() (cron.Schedule, error) {
	if w.Zone == "" || w.Zone == "Local" || len(strings.Fields(w.Cron)) != 5 {
		return nil, errors.New("schedule requires five cron fields and an IANA timezone")
	}
	if _, err := time.LoadLocation(w.Zone); err != nil {
		return nil, err
	}
	return cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse("CRON_TZ=" + w.Zone + " " + w.Cron)
}
func (b *Bridge) configureTrigger(f *Flow, in flowInput, rev int, p Principal) error {
	if rev == 0 {
		f.Instance = uuid.NewString()
		if in.When == nil {
			return errors.New("when required")
		}
	}
	if w := in.When; w != nil {
		switch w.Type {
		case "schedule":
			if w.Source != "" || w.Event != "" || w.Mode != "" || w.Filter != nil {
				return errors.New("event fields on schedule")
			}
			if _, err := w.schedule(); err != nil {
				return err
			}
			if rev > 0 && f.Schedule == nil {
				return errors.New("use a new trigger for a different schedule")
			}
			f.resetClock = !equal(f.Schedule, w)
			f.Schedule = w
		case "event":
			if w.Source == "" || w.Event == "" || w.Cron != "" || w.Zone != "" {
				return errors.New("expected source_id and event_name")
			}
			if rev > 0 && (f.Schedule != nil || w.Source != f.Source || w.Event != f.Name || w.Mode != "" && w.Mode != f.Mode || w.Filter != nil && f.Mode != "poll" && !equal(w.Filter, f.Filter)) {
				return errors.New("use a new trigger for a different event or ingestion mode")
			}
			f.Source = w.Source
			f.Name = w.Event
			if w.Mode != "" {
				f.Mode = w.Mode
			}
			if w.Filter != nil {
				f.Filter = w.Filter
			}
		default:
			return errors.New("when.type must be event or schedule")
		}
	}
	if in.Handler != nil {
		if in.Handler.Revision < 1 {
			return errors.New("handler requires a fixed revision")
		}
		def, _, err := b.readVersion(*in.Handler, p)
		if err != nil {
			return err
		}
		if err = b.checkBackgroundTools(p, def); err != nil {
			return err
		}
		f.Handler = in.Handler
	}
	if in.Replay != nil {
		f.Replay = *in.Replay
	}
	if in.Promotion != nil {
		f.Promotion = in.Promotion
	}
	if f.Promotion != nil && f.Promotion.Enabled {
		if f.Handler == nil || !f.Replay || len(f.Promotion.Holdout) == 0 || (f.Promotion.Metric != "reads" && f.Promotion.Metric != "output_bytes") {
			return errors.New("automatic promotion requires handler, replay, independent holdout and metric")
		}
	}
	return nil
}

func triggerView(f Flow, rev int) Object {
	var w any = f.Schedule
	if f.Schedule == nil {
		w = triggerWhen{Type: "event", Source: f.Source, Event: f.Name, Filter: f.Filter, Mode: f.Mode}
	}
	return Object{"trigger_id": f.ID, "revision": rev, "when": w, "destination_id": f.Destination, "disabled": f.Disabled, "handler": f.Handler, "skill_ref": f.Skill, "instruction": f.Instruction, "replay": f.Replay, "automatic_promotion": f.Promotion != nil && f.Promotion.Enabled}
}

func (b *Bridge) triggerQueries(v *view) {
	p := v.user
	tool(v.server, "list_triggers", "List your triggers and their current revisions.", func(_ context.Context, in struct {
		Offset int `json:"offset,omitempty"`
	}) (any, error) {
		list, err := storage.List[Flow](b.store, "flow")
		if err != nil {
			return nil, err
		}
		items := []Object{}
		for _, f := range list {
			if f.Owner == p.Name {
				rev, err := b.store.Get("flow", f.ID, &f)
				if err != nil {
					return nil, err
				}
				if f.Owner == p.Name {
					items = append(items, triggerView(f, rev))
				}
			}
		}
		batch, next := page(items, in.Offset)
		return Object{"triggers": batch, "next_offset": next}, nil
	})
	tool(v.server, "get_trigger", "Inspect a trigger and recent deliveries, or a specific run including retained replay evidence.", func(_ context.Context, in struct {
		ID  string `json:"trigger_id"`
		Run string `json:"run_id,omitempty"`
	}) (any, error) {
		if in.Run != "" {
			return b.runDetails(p, in.ID, in.Run)
		}
		var f Flow
		rev, err := b.store.Get("flow", in.ID, &f)
		if err != nil || f.Owner != p.Name {
			return nil, errors.New("trigger unavailable")
		}
		rows, err := b.store.DB.Query("SELECT d.id,d.status,d.attempt,d.error,COALESCE(l.kind,'webhook') FROM deliveries d LEFT JOIN local_deliveries l ON l.id=d.id WHERE flow=? ORDER BY d.rowid DESC LIMIT 50", f.ID)
		if err != nil {
			return nil, err
		}
		deliveries := []Object{}
		for rows.Next() {
			var id, status, msg, kind string
			var n int
			if err = rows.Scan(&id, &status, &n, &msg, &kind); err != nil {
				rows.Close()
				return nil, err
			}
			deliveries = append(deliveries, Object{"id": id, "status": status, "attempts": n, "error": msg, "destination_type": kind})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		rows, err = b.store.DB.Query("SELECT id,status FROM trigger_runs WHERE instance=? ORDER BY rowid DESC LIMIT 50", f.Instance)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		runs := []Object{}
		for rows.Next() {
			var id, status string
			if err = rows.Scan(&id, &status); err != nil {
				return nil, err
			}
			runs = append(runs, Object{"run_id": id, "status": status})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		out := Object{"trigger": triggerView(f, rev), "revision": rev, "runs": runs, "deliveries": deliveries}
		var candidate int
		var decision string
		err = b.store.DB.QueryRow("SELECT candidate,result FROM trigger_reviews WHERE instance=? ORDER BY rowid DESC LIMIT 1", f.Instance).Scan(&candidate, &decision)
		if err == nil {
			out["review"] = Object{"revision": candidate, "decision": decision}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return out, nil
	})
	tool(v.server, "delete_trigger", "Retire a trigger by ID and revision, retaining its execution history.", func(_ context.Context, in struct {
		ID       string `json:"trigger_id"`
		Revision int    `json:"revision"`
	}) (any, error) {
		b.modify.Lock()
		defer b.modify.Unlock()
		var f Flow
		rev, err := b.store.Get("flow", in.ID, &f)
		if err != nil || rev != in.Revision || f.Owner != p.Name {
			return nil, errors.New("trigger unavailable or revision conflict")
		}
		tx, err := b.store.DB.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if err = b.store.PutTx(tx, p.Name, "flow", f.ID, nil, rev); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("DELETE FROM objects WHERE kind IN ('cursor','upstream-hook') AND id=?", f.ID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("DELETE FROM trigger_clock WHERE instance=?", f.Instance); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("UPDATE trigger_runs SET status='cancelled' WHERE instance=? AND status='pending'", f.Instance); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("UPDATE deliveries SET status='suppressed' WHERE flow=? AND status='pending'", f.ID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("UPDATE deliveries SET flow=? WHERE flow=?", "archived/"+f.Instance+"/"+f.ID, f.ID); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		b.pollSchedule.Delete(f.ID)
		if stop, ok := b.activeStreams.Load(f.ID); ok {
			stop.(context.CancelFunc)()
		}
		return Object{"trigger_id": f.ID, "deleted": true}, nil
	})
}

// Upgrade in place so upstream cursors and outstanding delivery identifiers stay valid.
func (b *Bridge) migrateTriggers() error {
	tx, err := b.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	flows, err := storage.ListTx[Flow](b.store, tx, "flow")
	if err != nil {
		return err
	}
	for _, f := range flows {
		if f.Instance != "" {
			continue
		}
		f.Instance = uuid.NewString()
		raw, err := b.store.Seal("flow", f.ID, f)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE objects SET data=? WHERE kind='flow' AND id=?", raw, f.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type invocation struct {
	Args        Object       `json:"arguments"`
	Handler     *toolVersion `json:"handler,omitempty"`
	Destination string       `json:"destination_id"`
	Instruction string       `json:"instruction,omitempty"`
	Skill       string       `json:"skill_ref,omitempty"`
	Replay      bool         `json:"replay,omitempty"`
}

func (in invocation) deliveryBody(id, trigger string) Object {
	body := Object{"trigger_id": trigger, "run_id": id, "flow_id": trigger, "delivery_id": id, "skill_ref": in.Skill}
	if in.Instruction != "" {
		body["instruction"] = in.Instruction
	}
	return body
}

func (b *Bridge) queueTrigger(tx *sql.Tx, f Flow, key string, args Object) error {
	instance := f.Instance
	if instance == "" {
		instance = f.ID
	}
	id := digest([]byte(instance + "\x00" + f.Source + "\x00" + key))
	args["trigger_id"] = f.ID
	args["run_id"] = id
	in := invocation{Args: args, Handler: f.Handler, Destination: f.Destination, Skill: f.Skill, Instruction: f.Instruction, Replay: f.Replay}
	raw, err := b.store.Seal("run", id, in)
	if err != nil {
		return err
	}
	state := "pending"
	if f.Handler == nil {
		state = "processed"
	}
	r, err := tx.Exec("INSERT OR IGNORE INTO trigger_runs VALUES(?,?,?,?,?,?,NULL,unixepoch())", id, instance, f.ID, f.Owner, state, raw)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 || f.Handler != nil {
		return err
	}
	body := in.deliveryBody(id, f.ID)
	for k, v := range args {
		body[k] = v
	}
	return b.enqueueDelivery(tx, id, f.ID, f.Destination, body)
}
func (b *Bridge) scheduleDue(now time.Time) error {
	tx, err := b.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	flows, err := storage.ListTx[Flow](b.store, tx, "flow")
	if err != nil {
		return err
	}
	for _, f := range flows {
		if f.Schedule == nil || f.Disabled {
			continue
		}
		var checked int64
		if err = tx.QueryRow("SELECT checked FROM trigger_clock WHERE instance=?", f.Instance).Scan(&checked); err != nil {
			return err
		}
		schedule, err := f.Schedule.schedule()
		if err != nil {
			return err
		}
		// Find the most recent match by widening the lookback only for sparse schedules.
		start := now.Add(-time.Minute)
		for start.Unix() > checked {
			next := schedule.Next(start)
			if !next.IsZero() && !next.After(now) {
				break
			}
			start = start.Add(-now.Sub(start))
		}
		if start.Unix() < checked {
			start = time.Unix(checked, 0)
		}
		var latest time.Time
		for t := schedule.Next(start); !t.IsZero() && !t.After(now); t = schedule.Next(t) {
			latest = t
		}
		if !latest.IsZero() {
			stamp := latest.UTC().Format(time.RFC3339)
			if err = b.queueTrigger(tx, f, stamp, Object{"scheduled_at": stamp}); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("UPDATE trigger_clock SET checked=? WHERE instance=?", now.Unix(), f.Instance); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (b *Bridge) runDetails(p Principal, trigger, id string) (Object, error) {
	var raw, out []byte
	var status string
	err := b.store.DB.QueryRow("SELECT status,input,output FROM trigger_runs WHERE id=? AND trigger_id=? AND owner=?", id, trigger, p.Name).Scan(&status, &raw, &out)
	if err != nil {
		return nil, err
	}
	var in invocation
	if err = b.store.Decode("run", id, raw, &in); err != nil {
		return nil, err
	}
	r := Object{"run_id": id, "status": status, "handler": in.Handler}
	if in.Replay {
		r["arguments"] = in.Args
	}
	if out != nil {
		var result Object
		if err = b.store.Decode("out", id, out, &result); err != nil {
			return nil, err
		}
		r["processing"] = result
	}
	var delivery string
	err = b.store.DB.QueryRow("SELECT status FROM deliveries WHERE id=?", id).Scan(&delivery)
	if err == nil {
		r["delivery"] = delivery
		var kind string
		var blob []byte
		err = b.store.DB.QueryRow("SELECT kind,result FROM local_deliveries WHERE id=?", id).Scan(&kind, &blob)
		if errors.Is(err, sql.ErrNoRows) {
			kind = "webhook"
		} else if err != nil {
			return nil, err
		}
		r["destination_type"] = kind
		if len(blob) > 0 {
			var result Object
			if err = b.store.Decode("delivery-result", id, blob, &result); err != nil {
				return nil, err
			}
			r["delivery_result"] = result
		}

	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return r, nil
}
