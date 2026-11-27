package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Commit the verified publication and its directed webhook delivery together.
// The destination and thread come from the stored trigger, never the publisher.
func (b *Bridge) publishForTrigger(ctx context.Context, actor string, def Binding, revision int, trigger string) error {
	tx, err := b.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var flow, dest string
	var raw []byte
	if err = tx.QueryRow("SELECT flow,destination,data FROM deliveries WHERE id=?", trigger).Scan(&flow, &dest, &raw); err != nil {
		return err
	}
	var origin struct {
		Source string     `json:"source_id"`
		Event  Occurrence `json:"event"`
	}
	if err = b.store.Decode("delivery", trigger, raw, &origin); err != nil {
		return err
	}
	scope, _ := origin.Event.Data["task_scope"].(string)
	principal, _ := origin.Event.Data["actor"].(string)
	parts := strings.Split(scope, ":")
	if origin.Source != "bridge.builtin" || origin.Event.Name != taskReadEvent || principal == "" || len(parts) != 3 || parts[0] != "codex" || !ValidTaskScope(parts[1]) || !ValidTaskScope(parts[2]) {
		return errors.New("expected a Codex repeated-read trigger delivery")
	}
	if err = b.store.PutTx(tx, actor, "binding", def.ID, def, revision); err != nil {
		return err
	}
	if err = tx.QueryRow("SELECT revision FROM objects WHERE kind='binding' AND id=?", def.ID).Scan(&revision); err != nil {
		return err
	}
	id := digest([]byte(fmt.Sprintf("catalog.updated/%s/%s/%d", trigger, def.ID, revision)))
	notice := Occurrence{ID: id, Name: "catalog.updated", Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Data: Object{
		"origin_delivery_id": trigger, "thread_id": parts[1], "turn_id": parts[2], "actor": principal,
		"tool_id": "bridge.builtin/" + def.ID, "revision": revision,
	}}
	body := Object{"delivery_id": id, "flow_id": flow, "source_id": "bridge.builtin", "event": notice}
	if err = b.enqueueDelivery(tx, id, flow, dest, body); err != nil {
		return err
	}
	return tx.Commit()
}
