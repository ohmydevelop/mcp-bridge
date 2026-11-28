package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"newmcp/mcp-bridge/internal/storage"
)

type Object = map[string]any
type Principal struct {
	Name string
}
type Profile struct {
	Producer string `json:"producer_source,omitempty"`
}
type Source struct {
	OAuth   *sourceOAuth        `json:"oauth,omitempty"`
	Owner   string              `json:"owner,omitempty"`
	Events  map[string]string   `json:"events,omitempty"`
	Aliases map[string][]string `json:"aliases,omitempty"`

	ID       string            `json:"id"`
	Type     string            `json:"type,omitempty"`
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	URL      string            `json:"url,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	Package  string            `json:"package,omitempty"`
	Server   string            `json:"server,omitempty"`
	Settings Object            `json:"settings,omitempty"`
	Grants   []string          `json:"grants,omitempty"`
	Hidden   []string          `json:"hidden,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}
type backend struct {
	oauth           *protectedOAuth
	transportSource Source
	hub             *streamHub
	templates       []*mcp.ResourceTemplate
	resources       []*mcp.Resource
	prompts         []*mcp.Prompt
	session         *mcp.ClientSession
	tools           map[string]*mcp.Tool
}
type view struct {
	reserved  map[string]bool
	scope     string
	fileTools map[string]string
	prompts   []string
	user      Principal
	server    *mcp.Server
	templates []string
	uris      []string
}
type Bridge struct {
	scopedViews             map[[2]string]*view
	oauthStates, oauthFlows sync.Map
	catalogChanged          chan struct{}
	scriptsMu               sync.RWMutex
	registerApp             func(*mcp.Server)
	profiles                map[string]Profile
	presets                 map[string]Destination
	terminals               sync.Map
	verified                sync.Map
	activeStreams           sync.Map
	pollSchedule            sync.Map
	streamWG                sync.WaitGroup
	store                   *storage.Store
	dir                     string
	// Serializes reconfiguration and publication of a new catalog.
	modify  sync.Mutex
	mu      sync.RWMutex
	sources map[string]Source
	live    map[string]*backend
	views   map[Principal]*view
	ctx     context.Context
}

var identifier = regexp.MustCompile("^[a-z][a-z0-9-]{0,47}$")

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func openBridge(ctx context.Context, dir string, key []byte) (*Bridge, error) {
	store, err := storage.Open(dir, key)
	if err != nil {
		return nil, err
	}
	b := &Bridge{catalogChanged: make(chan struct{}, 1), store: store, dir: dir, ctx: ctx, sources: map[string]Source{}, live: map[string]*backend{}, views: map[Principal]*view{}}
	rows, err := storage.List[Source](b.store, "source")
	if err != nil {
		store.DB.Close()
		return nil, err
	}
	for _, r := range rows {
		b.sources[r.ID] = r
	}
	return b, nil
}
func member(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

// Pre-unification sources and destinations were created by the local identity.
const localIdentity = "admin"

func canUse(name, owner string, grants []string) bool {
	if owner == "" {
		owner = localIdentity
	}
	return name == owner || member(grants, name)
}
func allowed(p Principal, s Source) bool { return canUse(p.Name, s.Owner, s.Grants) }
