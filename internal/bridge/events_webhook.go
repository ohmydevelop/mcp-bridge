package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"newmcp/mcp-bridge/internal/storage"
)

type callback struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}
type subscription struct {
	ID, Source, Name, Owner, Cursor, Expires string
	Arguments                                Object
	Delivery                                 callback
}

func signingKey(value string) ([]byte, error) {
	if !strings.HasPrefix(value, "whsec_") {
		return nil, errors.New("invalid webhook key")
	}
	key, e := base64.StdEncoding.DecodeString(value[6:])
	if e != nil || len(key) < 24 || len(key) > 64 {
		return nil, errors.New("invalid webhook key")
	}
	return key, nil
}
func signature(key []byte, id, stamp string, data []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + stamp + "."))
	mac.Write(data)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
func signedRequest(ctx context.Context, d callback, subID, messageID string, data []byte) (*http.Request, error) {
	key, err := signingKey(d.Secret)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", d.URL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", messageID)
	req.Header.Set("webhook-timestamp", stamp)
	req.Header.Set("webhook-signature", signature(key, messageID, stamp, data))
	req.Header.Set("X-MCP-Subscription-Id", subID)
	return req, nil
}

func (b *Bridge) verifyReceiver(ctx context.Context, owner, id string, d callback) error {
	k := owner + "\x00" + d.URL
	if expiry, ok := b.verified.Load(k); ok && time.Now().Before(expiry.(time.Time)) {
		return nil
	}
	challenge := uuid.NewString()
	raw, _ := json.Marshal(Object{"type": "verification", "challenge": challenge})
	req, err := signedRequest(ctx, d, id, "msg_verification_"+uuid.NewString(), raw)
	if err != nil {
		return err
	}
	resp, err := webhookClient.Do(req)
	if err != nil {
		return errors.New("callback verification unavailable")
	}
	defer resp.Body.Close()
	var reply struct{ Challenge string }
	if resp.StatusCode/100 != 2 || json.NewDecoder(io.LimitReader(resp.Body, 2048)).Decode(&reply) != nil || subtle.ConstantTimeCompare([]byte(challenge), []byte(reply.Challenge)) != 1 {
		return errors.New("callback challenge failed")
	}
	b.verified.Store(k, time.Now().Add(time.Hour))
	return nil
}
func (b *Bridge) webhookMethods(v *view) {
	for _, method := range []string{"events/subscribe", "events/unsubscribe"} {
		native(v, method, func(ctx context.Context, in *extParams) (Object, error) {
			src, name, e := b.localType(v.user, in.Name)
			if e != nil {
				return nil, e
			}
			d := in.Delivery
			if d == nil || d.Mode != "webhook" || !strings.HasPrefix(d.URL, "https://") || !permittedURL(d.URL) {
				return nil, errors.New("webhook requires an approved HTTPS origin")
			}
			args := in.Arguments
			if args == nil {
				args = Object{}
			}
			key, _ := json.Marshal([]any{v.user.Name, d.URL, in.Name, args})
			id := "sub_" + digest(key)
			var rec subscription
			rev, e := b.store.Get("subscription", id, &rec)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return nil, e
			}
			if method == "events/unsubscribe" {
				if rev == 0 {
					return nil, errors.New("unknown subscription")
				}
				return Object{}, b.removeSubscription(v.user.Name, id, rev)
			}
			if _, e = signingKey(d.Secret); e != nil {
				return nil, e
			}
			if e = b.verifyReceiver(ctx, v.user.Name, id, *d); e != nil {
				return nil, e
			}
			ttl := time.Hour
			if in.TTL != nil {
				if *in.TTL < 1 {
					return nil, errors.New("invalid ttl")
				}
				ttl = time.Duration(min(*in.TTL, 3600000)) * time.Millisecond
			}
			if rev == 0 {
				state, e := b.pollLocal(v.user, src, name, args, in.Cursor, 1)
				if e != nil {
					return nil, e
				}
				cur, _ := state["cursor"].(string)
				if in.Cursor != nil {
					cur, _ = in.Cursor.(string)
				}
				rec = subscription{ID: id, Source: src, Name: name, Owner: v.user.Name, Cursor: cur, Arguments: args}
			}
			rec.Delivery = *d
			rec.Expires = time.Now().Add(ttl).UTC().Format(time.RFC3339Nano)
			if e = b.store.Put(v.user.Name, "subscription", id, rec, rev); e != nil {
				return nil, e
			}
			return Object{"id": id, "cursor": rec.Cursor, "truncated": false, "refreshBefore": rec.Expires}, nil
		})
	}
}
func (b *Bridge) removeSubscription(owner, id string, rev int) error {
	tx, err := b.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = b.store.PutTx(tx, owner, "subscription", id, nil, rev); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM deliveries WHERE flow='' AND destination=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *Bridge) enqueueSubscribed(ctx context.Context) error {
	records, e := storage.List[subscription](b.store, "subscription")
	if e != nil {
		return e
	}
	for _, rec := range records {
		p := Principal{rec.Owner}
		rev, e := b.store.Get("subscription", rec.ID, &rec)
		if e != nil {
			continue
		}
		expires, e := time.Parse(time.RFC3339Nano, rec.Expires)
		if e != nil {
			return e
		}
		if time.Now().After(expires) || !b.visible(p, rec.Source) {
			_ = b.removeSubscription(p.Name, rec.ID, rev)
			continue
		}
		if !permittedURL(rec.Delivery.URL) {
			continue
		}
		state, e := b.pollLocal(p, rec.Source, rec.Name, rec.Arguments, rec.Cursor, 1)
		if e != nil {
			return e
		}
		next := state["cursor"].(string)
		if next == rec.Cursor {
			continue
		}
		tx, err := b.store.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = func() error {
			defer tx.Rollback()
			var pending bool
			if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM deliveries WHERE flow='' AND destination=?)", rec.ID).Scan(&pending); err != nil || pending {
				return err
			}
			if events := state["events"].([]Occurrence); len(events) > 0 {
				ev := events[0]
				ev.Name = rec.Source + "__" + ev.Name
				id := "msg_" + digest([]byte(rec.ID+"/"+ev.ID))
				if err := b.enqueueDelivery(tx, id, "", rec.ID, ev); err != nil {
					return err
				}
			}
			rec.Cursor = next
			if err := b.store.PutTx(tx, p.Name, "subscription", rec.ID, rec, rev); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			return err
		}
	}
	return nil
}
