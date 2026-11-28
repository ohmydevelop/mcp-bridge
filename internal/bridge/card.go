package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"newmcp/mcp-bridge/internal/mcpinfo"
)

const cardSchema = "https://static.modelcontextprotocol.io/schemas/v1/server-card.schema.json"
const cardMedia = "application/mcp-server-card+json"

func webEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || !(u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())) {
		return nil, errors.New("expected HTTPS or a loopback HTTP address")
	}
	return u, nil
}

func registerCards(mux *http.ServeMux) {
	base := strings.TrimRight(os.Getenv("BRIDGE_PUBLIC_URL"), "/")
	u, err := webEndpoint(base)
	if err != nil || u.RawQuery != "" {
		return
	}
	name := u.Hostname() + "/mcp-bridge"
	card := Object{"$schema": cardSchema, "name": name, "version": mcpinfo.Implementation.Version, "description": "Discover and call tools, resources, prompts and automations from connected MCP servers.", "remotes": []Object{{"type": "streamable-http", "url": base + "/mcp", "supportedProtocolVersions": []string{modernVersion}}}}
	entry := Object{"identifier": "urn:air:" + u.Hostname() + ":mcp:bridge", "type": cardMedia, "url": base + "/mcp/server-card"}
	for route, response := range map[string]struct {
		media string
		body  Object
	}{
		"GET /oauth/client.json":           {"application/json", Object{"client_id": base + "/oauth/client.json", "client_name": "MCP Bridge", "redirect_uris": []string{base + "/oauth/callback"}, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "token_endpoint_auth_method": "none"}},
		"GET /mcp/server-card":             {cardMedia, card},
		"GET /.well-known/ai-catalog.json": {"application/ai-catalog+json", Object{"specVersion": "1.0", "entries": []Object{entry}}},
	} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", response.media)
			_ = json.NewEncoder(w).Encode(response.body)
		})
	}
}

func readDiscovery(ctx context.Context, raw string) (Object, error) {
	u, err := webEndpoint(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", cardMedia+", application/ai-catalog+json, application/json")
	res, err := (&http.Client{Timeout: 8 * time.Second, CheckRedirect: rejectRedirect}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errors.New("discovery document unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	var doc Object
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &doc) != nil || doc == nil {
		return nil, errors.New("invalid discovery document")
	}
	return doc, nil
}

func (b *Bridge) discoveryTool(v *view) {
	if v.user.Name != localIdentity {
		return
	}
	tool(v, "bridge_discover_server", "Inspect a domain's AI Catalog or an explicit MCP Server Card before connecting. Use the returned remote URL with bridge_create_source.", func(ctx context.Context, in struct {
		URL string `json:"url"`
	}) (any, error) {
		u, err := webEndpoint(in.URL)
		if err != nil {
			return nil, err
		}
		if u.Path == "" || u.Path == "/" {
			u.Path = "/.well-known/ai-catalog.json"
		}
		doc, err := readDiscovery(ctx, u.String())
		if err != nil {
			return nil, err
		}
		if doc["$schema"] == cardSchema {
			return doc, nil
		}
		entries, ok := doc["entries"].([]any)
		if !ok || doc["specVersion"] != "1.0" {
			return nil, invalid("unsupported discovery document")
		}
		cards := []Object{}
		for _, entry := range entries {
			obj, _ := entry.(map[string]any)
			if obj["type"] == cardMedia {
				cards = append(cards, obj)
			}
		}
		return Object{"cards": cards}, nil
	})
}
