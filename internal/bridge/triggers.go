package bridge

import (
	"errors"
	"strings"
)

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
