package bridge

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"

	"strings"

	"newmcp/mcp-bridge/internal/plugin"
)

type sourceRevision struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

var secretName = regexp.MustCompile("^[A-Z][A-Z0-9_]{0,63}$")

func secretValue(name string) (string, error) {
	if !secretName.MatchString(name) {
		return "", errors.New("invalid credential reference")
	}
	v, ok := os.LookupEnv("BRIDGE_CRED_" + name)
	if !ok {
		return "", fmt.Errorf("credential %s unavailable", name)
	}
	return v, nil
}
func configValue(v any, def plugin.Setting) (string, error) {
	if def.Sensitive {
		ref, ok := v.(map[string]any)
		if !ok {
			return "", errors.New("expected credential reference")
		}
		name, ok := ref["credential"].(string)
		if !ok {
			return "", errors.New("expected credential reference")
		}
		return secretValue(name)
	}
	if def.Multiple {
		if vals, ok := v.([]any); ok {
			parts := []string{}
			for _, x := range vals {
				st, ok := x.(string)
				if !ok {
					return "", errors.New("expected strings")
				}
				parts = append(parts, st)
			}
			return strings.Join(parts, string(os.PathListSeparator)), nil
		}
		return "", errors.New("expected multiple paths")
	}
	switch def.Type {
	case "string", "file", "directory":
		if s, ok := v.(string); ok {
			return s, nil
		}
	case "number":
		if s, ok := v.(float64); ok {
			return fmt.Sprint(s), nil
		}
	case "boolean":
		if s, ok := v.(bool); ok {
			return fmt.Sprint(s), nil
		}
	}
	return "", errors.New("setting type mismatch")
}
func sameConnection(a, b Source) bool {
	return a.Type == b.Type && a.Command == b.Command && a.URL == b.URL && a.Cwd == b.Cwd && reflect.DeepEqual(a.Args, b.Args) && reflect.DeepEqual(a.Env, b.Env) && reflect.DeepEqual(a.Headers, b.Headers)
}
