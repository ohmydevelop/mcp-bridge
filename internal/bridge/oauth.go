package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/auth/extauth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

type sourceOAuth struct {
	Mode         string   `json:"mode"`
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	IdPIssuer    string   `json:"idp_issuer,omitempty"`
	IdPClientID  string   `json:"idp_client_id,omitempty"`
	IdPSecret    string   `json:"idp_client_secret,omitempty"`
	IDToken      string   `json:"id_token,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

func credentials(issuer, id, secret string) *oauthex.ClientCredentials {
	c := &oauthex.ClientCredentials{Issuer: issuer, ClientID: id}
	if secret != "" {
		c.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
	}
	return c
}
func (o *sourceOAuth) validate(s Source) error {
	if o == nil {
		return nil
	}
	if s.Type != "streamable-http" || (o.Mode != "authorization_code" && o.ClientID == "") {
		return errors.New("OAuth requires streamable-http and client_id")
	}
	urls := []string{s.URL}
	if o.ClientID != "" || o.Issuer != "" {
		urls = append(urls, o.Issuer)
	}
	for _, str := range urls {
		if _, err := webEndpoint(str); err != nil {
			return err
		}
	}
	for k := range s.Headers {
		if strings.EqualFold(k, "Authorization") {
			return errors.New("OAuth and a fixed Authorization header conflict")
		}
	}
	switch o.Mode {
	case "authorization_code":
		callback, err := oauthCallbackURL()
		if err == nil && o.ClientID == "" && !strings.HasPrefix(callback, "https://") {
			return errors.New("client metadata discovery requires an HTTPS BRIDGE_PUBLIC_URL or a preregistered client_id")
		}
		return err
	case "client_credentials":
		if o.ClientSecret == "" {
			return errors.New("client_secret required")
		}
	case "enterprise":
		if _, err := webEndpoint(o.IdPIssuer); err != nil {
			return err
		}
		if o.IdPClientID == "" || (o.IDToken == "" && enterpriseTokenFile(s) == "") {
			return errors.New("enterprise requires idp_client_id and an IdP-issued id_token")
		}
	default:
		return errors.New("unknown OAuth mode")
	}
	return nil
}
func oauthCallbackURL() (string, error) {
	base := strings.TrimRight(os.Getenv("BRIDGE_PUBLIC_URL"), "/")
	u, err := webEndpoint(base)
	if err != nil || u.RawQuery != "" {
		return "", errors.New("set BRIDGE_PUBLIC_URL for the OAuth browser callback")
	}
	return base + "/oauth/callback", nil
}

type savedGrant struct {
	Config oauth2.Config
	Token  oauth2.Token
}
type persistedToken struct {
	mu          sync.Mutex
	b           *Bridge
	source, key string
	grant       savedGrant
	tokens      oauth2.TokenSource
}

func (p *persistedToken) Token() (*oauth2.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, err := p.tokens.Token()
	if err != nil {
		return nil, err
	}
	if p.grant.Token.AccessToken != t.AccessToken || p.grant.Token.RefreshToken != t.RefreshToken || !p.grant.Token.Expiry.Equal(t.Expiry) {
		next := savedGrant{p.grant.Config, *t}
		if err = p.b.saveGrant(p.source, p.key, next, &p.grant); err != nil {
			return nil, err
		}
		p.grant = next
	}
	return t, nil
}
func oauthKey(s Source) string {
	raw, _ := json.Marshal([]any{s.Owner, s.URL, s.OAuth})
	return digest(raw)
}
func (b *Bridge) saveGrant(source, key string, grant savedGrant, previous *savedGrant) error {
	data, err := b.store.Seal("upstream-oauth", source+key, grant)
	if err != nil {
		return err
	}
	if previous == nil {
		_, err = b.store.DB.Exec("INSERT INTO oauth_grants(source,key,data) VALUES(?,?,?) ON CONFLICT(source) DO UPDATE SET key=excluded.key,data=excluded.data", source, key, data)
		return err
	}
	var old []byte
	if err = b.store.DB.QueryRow("SELECT data FROM oauth_grants WHERE source=? AND key=?", source, key).Scan(&old); err != nil {
		return err
	}
	var stored savedGrant
	if err = b.store.Decode("upstream-oauth", source+key, old, &stored); err != nil {
		return err
	}
	if stored.Token.AccessToken != previous.Token.AccessToken || stored.Token.RefreshToken != previous.Token.RefreshToken || !stored.Token.Expiry.Equal(previous.Token.Expiry) {
		return errors.New("OAuth grant was replaced")
	}
	res, err := b.store.DB.Exec("UPDATE oauth_grants SET data=? WHERE source=? AND key=? AND data=?", data, source, key, old)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("OAuth grant changed")
	}
	return nil
}

var loginRequired = errors.New("user authorization required; call bridge_begin_oauth")

type protectedOAuth struct {
	mu      sync.Mutex
	handler auth.OAuthHandler
}

func (p *protectedOAuth) Authorize(ctx context.Context, r *http.Request, resp *http.Response) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// The service-grant token source outlives the MCP request. Each HTTP
	// exchange has its own timeout on the configured client.
	if _, ok := p.handler.(*extauth.ClientCredentialsHandler); ok {
		ctx = context.WithoutCancel(ctx)
	}
	return p.handler.Authorize(ctx, r, resp)
}
func (p *protectedOAuth) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handler.TokenSource(ctx)
}
func (p *protectedOAuth) attach(r *http.Request) error {
	if p == nil {
		return nil
	}
	ts, err := p.TokenSource(r.Context())
	if err != nil {
		return err
	}
	if ts == nil {
		return loginRequired
	}
	t, err := ts.Token()
	if err == nil {
		t.SetAuthHeader(r)
	}
	return err
}

type authFlow struct {
	url    chan string
	result chan *auth.AuthorizationResult
	done   chan struct{}
	err    error
}

func (b *Bridge) oauthHandler(s Source, flow *authFlow) (*protectedOAuth, error) {
	o := s.OAuth
	if o == nil {
		return nil, nil
	}
	if err := o.validate(s); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: rejectRedirect}
	creds := credentials(o.Issuer, o.ClientID, o.ClientSecret)
	var handler auth.OAuthHandler
	var err error
	switch o.Mode {
	case "client_credentials":
		handler, err = extauth.NewClientCredentialsHandler(&extauth.ClientCredentialsHandlerConfig{Credentials: creds, HTTPClient: client})
	case "enterprise":
		handler, err = extauth.NewEnterpriseHandler(&extauth.EnterpriseHandlerConfig{IdPIssuerURL: o.IdPIssuer, IdPCredentials: credentials(o.IdPIssuer, o.IdPClientID, o.IdPSecret), MCPAuthServerURL: o.Issuer, MCPResourceURI: s.URL, MCPCredentials: creds, MCPScopes: o.Scopes, HTTPClient: client, IDTokenFetcher: func(context.Context) (*oauth2.Token, error) {
			assertion := o.IDToken
			if assertion == "" {
				file, err := os.Open(enterpriseTokenFile(s))
				if err != nil {
					return nil, errors.New("IdP token file unavailable")
				}
				defer file.Close()
				raw, err := io.ReadAll(io.LimitReader(file, 65537))
				if err != nil || len(raw) > 65536 {
					return nil, errors.New("invalid IdP token file")
				}
				assertion = strings.TrimSpace(string(raw))
			}
			return (&oauth2.Token{}).WithExtra(map[string]any{"id_token": assertion}), nil
		}})
	case "authorization_code":
		redirect, _ := oauthCallbackURL()
		key := oauthKey(s)
		makeTokens := func(g savedGrant) oauth2.TokenSource {
			return &persistedToken{b: b, source: s.ID, key: key, grant: g, tokens: g.Config.TokenSource(context.WithValue(b.ctx, oauth2.HTTPClient, client), &g.Token)}
		}
		cfg := &auth.AuthorizationCodeHandlerConfig{PreregisteredClient: creds, RedirectURL: redirect, Client: client, RequestRefreshToken: true, AcceptUnadvertisedIss: true}
		if o.ClientID == "" {
			cfg.PreregisteredClient = nil
			cfg.ClientIDMetadataDocumentConfig = &auth.ClientIDMetadataDocumentConfig{URL: strings.TrimSuffix(redirect, "/callback") + "/client.json"}
		}
		if len(o.Scopes) > 0 {
			cfg.ScopeFilter = func([]string) []string { return o.Scopes }
		}
		if flow == nil {
			var raw []byte
			err = b.store.DB.QueryRow("SELECT data FROM oauth_grants WHERE source=? AND key=?", s.ID, key).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, loginRequired
			}
			if err != nil {
				return nil, err
			}
			var g savedGrant
			if err = b.store.Decode("upstream-oauth", s.ID+key, raw, &g); err != nil {
				return nil, err
			}
			cfg.InitialTokenSource = makeTokens(g)
		}
		cfg.AuthorizationCodeFetcher = func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			if flow == nil {
				return nil, loginRequired
			}
			u, err := webEndpoint(args.URL)
			if err != nil {
				return nil, err
			}
			state := u.Query().Get("state")
			if state == "" {
				return nil, errors.New("authorization state missing")
			}
			if _, used := b.oauthStates.LoadOrStore(state, flow); used {
				return nil, errors.New("duplicate authorization state")
			}
			defer b.oauthStates.Delete(state)
			select {
			case flow.url <- args.URL:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case v := <-flow.result:
				if v == nil {
					return nil, errors.New("authorization declined")
				}
				return v, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		cfg.NewTokenSource = func(_ context.Context, c *oauth2.Config, t *oauth2.Token) (oauth2.TokenSource, error) {
			g := savedGrant{*c, *t}
			b.modify.Lock()
			defer b.modify.Unlock()
			var current Source
			if _, err := b.store.Get("source", s.ID, &current); err != nil || current.Disabled || oauthKey(current) != key {
				return nil, errors.New("source changed during authorization")
			}
			if err := b.saveGrant(s.ID, key, g, nil); err != nil {
				return nil, err
			}
			return makeTokens(g), nil
		}
		handler, err = auth.NewAuthorizationCodeHandler(cfg)
	}
	if err != nil {
		return nil, err
	}
	return &protectedOAuth{handler: handler}, nil
}

func (b *Bridge) oauthTool(v *view) {
	tool(v, "bridge_begin_oauth", "Get the upstream authorization URL for a saved authorization_code source. Open it in a browser to authorize the source.", func(ctx context.Context, in struct {
		ID string `json:"source_id"`
	}) (any, error) {
		var s Source
		if _, err := b.store.Get("source", in.ID, &s); err != nil || s.Disabled || !canUse(v.user.Name, s.Owner, nil) || s.OAuth == nil || s.OAuth.Mode != "authorization_code" {
			return nil, invalid("OAuth source unavailable")
		}
		f := &authFlow{url: make(chan string, 1), done: make(chan struct{}), result: make(chan *auth.AuthorizationResult, 1)}
		if _, used := b.oauthFlows.LoadOrStore(s.ID, f); used {
			return nil, invalid("authorization is already in progress")
		}
		go func() {
			defer b.oauthFlows.Delete(s.ID)
			defer close(f.done)
			life, end := context.WithTimeout(b.ctx, 5*time.Minute)
			defer end()
			h, err := b.oauthHandler(s, f)
			if err == nil {
				// Probe using a standard, credential-free discover POST to obtain the AS challenge.
				payload := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
				req, e := http.NewRequestWithContext(life, "POST", s.URL, strings.NewReader(payload))
				if e != nil {
					f.err = e
					return
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json,text/event-stream")
				req.Header.Set("Mcp-Method", "server/discover")
				req.Header.Set("Mcp-Protocol-Version", modernVersion)
				res, e := (&http.Client{Timeout: 12 * time.Second, CheckRedirect: rejectRedirect}).Do(req)
				if e != nil {
					f.err = e
					return
				}
				// A public discovery response is valid even when individual tools require authorization.
				err = h.Authorize(life, req, res)
				res.Body.Close()
			}
			if err == nil {
				b.modify.Lock()
				var current Source
				if _, err = b.store.Get("source", s.ID, &current); err == nil && !current.Disabled && oauthKey(current) == oauthKey(s) {
					var status any
					status, err = b.reconcile(life, current)
					if err == nil && status.(Object)["state"] != "ready" {
						err = errors.New("source reconnect failed")
					}
				} else {
					err = errors.New("source changed during authorization")
				}
				b.modify.Unlock()
			}
			f.err = err
		}()
		select {
		case address := <-f.url:
			return Object{"authorization_url": address, "source_id": s.ID}, nil
		case <-f.done:
			return nil, errors.New("could not start OAuth authorization")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
}

func (b *Bridge) oauthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	v, ok := b.oauthStates.LoadAndDelete(state)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !ok {
		http.Error(w, "invalid or expired authorization", 400)
		return
	}
	f := v.(*authFlow)
	var result *auth.AuthorizationResult
	if q.Get("error") == "" && q.Get("code") != "" {
		result = &auth.AuthorizationResult{State: state, Code: q.Get("code"), Iss: q.Get("iss")}
	}
	f.result <- result
	select {
	case <-f.done:
		if f.err != nil {
			http.Error(w, "authorization failed", 400)
			return
		}
		_, _ = w.Write([]byte("Authorization complete. You may close this window."))
	case <-r.Context().Done():
	}
}

// The operator binds a source ID to a renewable assertion from its identity
// provider. Remote users cannot choose a filesystem path or claim this identity.
func enterpriseTokenFile(s Source) string {
	if s.Owner != localIdentity {
		return ""
	}
	var files map[string]string
	if json.Unmarshal([]byte(os.Getenv("BRIDGE_OIDC_TOKEN_FILES")), &files) != nil {
		return ""
	}
	return files[s.ID]
}
