package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

type destinationInput struct {
	Destination *Destination `json:"destination,omitempty" jsonschema:"Full definition to create or replace."`
	ID          string       `json:"destination_id,omitempty" jsonschema:"Existing destination to patch."`
	Patch       Object       `json:"patch,omitempty" jsonschema:"Changed fields only. Arrays replace in full; null clears an optional field."`
	Revision    int          `json:"revision"`
}

func (b *Bridge) effectiveDestination(ctx context.Context, p Principal, id string) (Destination, int, error) {
	var d Destination
	rev, err := b.store.Get("destination", id, &d)
	local := ctx.Value(localConnectionKey{}) == true
	if err != nil || !(local || canUse(p.Name, d.Owner, nil)) || (d.kind() == "cli_process" && !local) {
		return Destination{}, 0, errors.New("destination configuration unavailable")
	}
	d.Type = d.kind()
	if d.Type == "cli_process" && d.Command == "" {
		if preset, ok := b.presets[id]; ok {
			d.Command, d.Cwd, d.Args, d.Format = preset.Command, preset.Cwd, preset.Args, preset.Format
		}
	}
	return d, rev, nil
}

func (b *Bridge) saveDestination(ctx context.Context, p Principal, in destinationInput) (any, error) {
	b.modify.Lock()
	defer b.modify.Unlock()
	var d Destination
	if in.Destination != nil && in.ID == "" && in.Patch == nil {
		d = *in.Destination
		d.Type = d.kind()
		d.Owner = p.Name
		if in.Revision > 0 {
			var previous Destination
			if _, err := b.store.Get("destination", d.ID, &previous); err != nil {
				return nil, err
			}
			if previous.kind() != d.Type {
				return nil, errors.New("destination type cannot change")
			}
			d.Owner = previous.Owner
		}
	} else if in.ID != "" && len(in.Patch) > 0 && in.Destination == nil && in.Revision > 0 {
		previous, rev, err := b.effectiveDestination(ctx, p, in.ID)
		if err != nil {
			return nil, err
		}
		if rev != in.Revision {
			return nil, errors.New("revision conflict; reload configuration")
		}
		for key := range in.Patch {
			switch key {
			case "command", "args", "cwd", "input_format", "url", "credential", "description", "grants":
			default:
				return nil, errors.New("unknown or immutable destination patch field: " + key)
			}
		}
		raw, err := json.Marshal(previous)
		if err != nil {
			return nil, err
		}
		var fields Object
		if err = json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		mergePatch(fields, in.Patch)
		raw, err = json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&d); err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("supply either destination, or destination_id with patch and current revision")
	}
	if in.Revision < 0 || !identifier.MatchString(d.ID) {
		return nil, errors.New("invalid destination id or revision")
	}
	if err := d.validate(ctx); err != nil {
		return nil, err
	}
	return Object{"revision": in.Revision + 1}, b.store.Put(p.Name, "destination", d.ID, d, in.Revision)
}

func (d Destination) validate(ctx context.Context) error {
	switch d.Type {
	case "cli_process":
		if ctx.Value(localConnectionKey{}) != true {
			return errors.New("CLI configuration requires local stdio")
		}
		if d.URL != "" || d.Credential != "" {
			return errors.New("webhook fields on CLI destination")
		}
		if err := d.validateCLI(); err != nil {
			return err
		}
	case "webhook":
		if d.Command != "" || d.Args != nil || d.Cwd != "" || d.Format != "" {
			return errors.New("CLI fields require type cli_process")
		}
		if !permittedURL(d.URL) {
			return errors.New("invalid destination origin")
		}
		if d.Credential != "" {
			if _, err := secretValue(d.Credential); err != nil {
				return err
			}
		}
	default:
		return errors.New("supported destination types: webhook, cli_process")
	}
	return nil
}
