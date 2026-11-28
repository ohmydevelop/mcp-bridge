package bridge

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// Keep the base64 request below the Bridge HTTP message limit, including its envelope.
const inlineBudget = 4 << 20

func inlineLimit(opts Object) int64 {
	n, _ := opts["maxInlineBytes"].(float64)
	return int64(max(0, min(n, inlineBudget)))
}

// Follow the tool's schema, rather than treating any object with an "id" as a file.
func fileInputs(schema any, args any, limit int64) (map[string]fileInfo, error) {
	root := settings(schema)
	pending := map[string]fileInfo{}
	seen := map[string]bool{}
	type position struct {
		path   string
		schema string
	}
	visited := map[position]bool{}
	var visit func(Object, any, string, string) error
	visit = func(s Object, value any, path, schemaPath string) error {
		if s == nil || value == nil {
			return nil
		}
		key := position{path, schemaPath}
		if visited[key] {
			return nil
		}
		visited[key] = true
		obj, _ := value.(map[string]any)
		if _, ok := s["x-mcp-file"].(map[string]any); ok && obj != nil && obj["id"] != nil && obj["size"] != nil {
			if seen[path] {
				return nil
			}
			seen[path] = true
			raw, _ := json.Marshal(obj)
			var f fileInfo
			if json.Unmarshal(raw, &f) != nil || f.ID == "" || len([]rune(f.ID)) > 128 || f.Size == nil || *f.Size < 0 {
				return errors.New("invalid file metadata")
			}
			idKey := "id:" + f.ID
			if seen[idKey] {
				return errors.New("duplicate file ID")
			}
			seen[idKey] = true
			desc, _ := s["x-mcp-file"].(map[string]any)
			if n, ok := desc["maxSize"].(float64); ok && float64(*f.Size) > n {
				return errors.New("file exceeds maxSize")
			}
			data, inline := obj["data"]
			if !inline && *f.Size > 0 {
				pending[f.ID] = f
				return nil
			}
			if *f.Size > limit {
				return errors.New("inline files exceed maxInlineBytes")
			}
			var bytes []byte
			if inline {
				encoded, ok := data.(string)
				if !ok || int64(len(encoded)) != 4*((*f.Size+2)/3) {
					return errors.New("invalid inline file data")
				}
				var err error
				bytes, err = base64.StdEncoding.Strict().DecodeString(encoded)
				if err != nil || int64(len(bytes)) != *f.Size {
					return errors.New("invalid inline file size or encoding")
				}
			}
			if f.Digest != nil {
				hash := sha256.Sum256(bytes)
				if f.Digest.Algorithm != "sha-256" || base64.RawURLEncoding.EncodeToString(hash[:]) != f.Digest.Value {
					return errors.New("file digest mismatch")
				}
			}
			limit -= *f.Size
			return nil
		}
		if ref, ok := s["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			dest := root
			for _, part := range strings.Split(ref[2:], "/") {
				dest, _ = dest[strings.NewReplacer("~1", "/", "~0", "~").Replace(part)].(map[string]any)
			}
			if err := visit(dest, value, path, ref); err != nil {
				return err
			}
		}
		for _, kind := range []string{"allOf", "anyOf", "oneOf"} {
			branches, _ := s[kind].([]any)
			for i, branch := range branches {
				sch, _ := branch.(map[string]any)
				if err := visit(sch, value, path, schemaPath+"/"+kind+"/"+strconv.Itoa(i)); err != nil {
					return err
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		for k, v := range obj {
			prop, ok := props[k]
			if !ok {
				prop = s["additionalProperties"]
			}
			sch, _ := prop.(map[string]any)
			part := strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
			if err := visit(sch, v, path+"/"+part, schemaPath+"/properties/"+part); err != nil {
				return err
			}
		}
		items, _ := s["items"].(map[string]any)
		arr, _ := value.([]any)
		for i, v := range arr {
			if err := visit(items, v, path+"/"+strconv.Itoa(i), schemaPath+"/items"); err != nil {
				return err
			}
		}
		return nil
	}
	err := visit(root, args, "", "#")
	return pending, err
}
