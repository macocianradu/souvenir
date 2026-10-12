package webapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/dbtest"
	"git.estatecloud.org/radumaco/souvenir/db/sessions"
	"git.estatecloud.org/radumaco/souvenir/db/users"
	"github.com/go-jose/go-jose/v4"
)

const clientId = "souvenir"

// provider is a minimal OIDC provider. The test plays the browser, so the
// authorize step is a direct call to authorize.
type provider struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	grants map[string]grant
}

type grant struct {
	challenge, nonce string
	claims           map[string]any
}

func newProvider(t *testing.T) *provider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &provider{t: t, key: key, grants: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.srv.URL,
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.mu.Lock()
		g, ok := p.grants[r.Form.Get("code")]
		delete(p.grants, r.Form.Get("code"))
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 300,
			"id_token": p.sign(g),
		})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *provider) sign(g grant) string {
	claims := map[string]any{
		"iss": p.srv.URL, "aud": clientId, "nonce": g.nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(),
	}
	for k, v := range g.claims {
		claims[k] = v
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: p.key, KeyID: "k1"}}, nil)
	if err != nil {
		p.t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := signer.Sign(payload)
	if err != nil {
		p.t.Fatal(err)
	}
	raw, _ := obj.CompactSerialize()
	return raw
}

// authorize stands in for the person signing in at the provider and
// returns the code it would redirect back with.
func (p *provider) authorize(authURL string, nonce string, claims map[string]any) string {
	u, err := url.Parse(authURL)
	if err != nil {
		p.t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != clientId {
		p.t.Fatalf("authorize request %v", q)
	}
	if nonce == "" {
		nonce = q.Get("nonce")
	}
	code := sessions.Token()
	p.mu.Lock()
	p.grants[code] = grant{challenge: q.Get("code_challenge"), nonce: nonce, claims: claims}
	p.mu.Unlock()
	return code
}

type harness struct {
	t        *testing.T
	provider *provider
	server   *Server
	handler  http.Handler
	accounts *users.Store
}

func newHarness(t *testing.T) *harness {
	pool := dbtest.Pool(t, "webapp")
	p := newProvider(t)
	accounts := users.NewStore(pool)
	s := New(
		config.WebConfig{Listen: ":0", BaseUrl: "http://souvenir.test", SessionDays: 1},
		config.OidcConfig{Issuer: p.srv.URL, ClientId: clientId, ClientSecret: "secret", MatchClaim: "preferred_username"},
		accounts, sessions.NewStore(pool))
	return &harness{t: t, provider: p, server: s, handler: s.Handler(), accounts: accounts}
}

func (h *harness) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func cookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

type signIn struct {
	claims      map[string]any
	nonce       string // a nonce the provider puts in the token instead of the right one
	loginCookie string // a login cookie the browser sends instead of its own
}

// signIn runs /login, the provider and the callback, and returns the
// callback's response.
func (h *harness) signIn(o signIn) *httptest.ResponseRecorder {
	h.t.Helper()
	login := h.do(httptest.NewRequest("GET", "/login", nil))
	if login.Code != http.StatusFound {
		h.t.Fatalf("login answered %d: %s", login.Code, login.Body)
	}
	state := cookie(login, "souvenir-login")
	if state == nil {
		h.t.Fatal("no login cookie")
	}
	code := h.provider.authorize(login.Header().Get("Location"), o.nonce, o.claims)
	cb := httptest.NewRequest("GET", callbackPath+"?code="+code+"&state="+url.QueryEscape(state.Value), nil)
	if o.loginCookie != "" {
		state.Value = o.loginCookie
	}
	cb.AddCookie(state)
	return h.do(cb)
}

func (h *harness) home(session *http.Cookie) string {
	r := httptest.NewRequest("GET", "/", nil)
	if session != nil {
		r.AddCookie(session)
	}
	return h.do(r).Body.String()
}

func dana(sub string) map[string]any {
	return map[string]any{"sub": sub, "preferred_username": "dana"}
}

func TestSignIn_OnlyExistingUsersLinkedOnceBySubject(t *testing.T) {
	h := newHarness(t)

	w := h.signIn(signIn{claims: dana("sub-1")})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "no account for dana") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if cookie(w, "souvenir-session") != nil {
		t.Fatal("a session was given without an account")
	}

	if _, err := h.accounts.Add(context.Background(), "dana", false); err != nil {
		t.Fatal(err)
	}
	w = h.signIn(signIn{claims: dana("sub-1")})
	session := cookie(w, "souvenir-session")
	if w.Code != http.StatusSeeOther || session == nil || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("%d %+v %s", w.Code, session, w.Body)
	}
	if !strings.Contains(h.home(session), "Signed in as dana") {
		t.Fatal("the session does not sign in")
	}

	// Once linked, the subject decides: a renamed account still signs in...
	w = h.signIn(signIn{claims: map[string]any{"sub": "sub-1", "preferred_username": "dana-renamed"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("linked login refused: %d %s", w.Code, w.Body)
	}
	// ...and another provider account calling itself dana cannot take over.
	w = h.signIn(signIn{claims: dana("sub-2")})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "already linked") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	if n, err := h.accounts.Unlink(context.Background(), "dana"); err != nil || n != 1 {
		t.Fatalf("unlinked %d: %v", n, err)
	}
	if strings.Contains(h.home(session), "Signed in") {
		t.Fatal("unlinking kept the session")
	}
	if w := h.signIn(signIn{claims: dana("sub-2")}); w.Code != http.StatusSeeOther {
		t.Fatalf("after unlinking, the next login should link: %d %s", w.Code, w.Body)
	}
}

func TestSignIn_RefusesForgedOrReplayedCallbacks(t *testing.T) {
	h := newHarness(t)
	h.accounts.Add(context.Background(), "dana", false)

	if w := h.signIn(signIn{claims: dana("s"), loginCookie: sessions.Token()}); w.Code != http.StatusBadRequest {
		t.Fatalf("a callback for another browser's sign-in: %d", w.Code)
	}
	if w := h.signIn(signIn{claims: dana("s"), nonce: "replayed"}); w.Code != http.StatusBadGateway || cookie(w, "souvenir-session") != nil {
		t.Fatalf("a token with the wrong nonce: %d", w.Code)
	}
	if w := h.signIn(signIn{claims: map[string]any{"sub": "s"}}); w.Code != http.StatusForbidden {
		t.Fatalf("a login without the match claim: %d", w.Code)
	}

	login := h.do(httptest.NewRequest("GET", "/login", nil))
	state := cookie(login, "souvenir-login")
	code := h.provider.authorize(login.Header().Get("Location"), "", dana("s"))
	callback := func() int {
		r := httptest.NewRequest("GET", callbackPath+"?code="+code+"&state="+url.QueryEscape(state.Value), nil)
		r.AddCookie(state)
		return h.do(r).Code
	}
	if c := callback(); c != http.StatusSeeOther {
		t.Fatalf("first use %d", c)
	}
	if c := callback(); c != http.StatusBadRequest {
		t.Fatalf("a replayed callback answered %d", c)
	}

	r := httptest.NewRequest("GET", callbackPath+"?error=access_denied&state=x", nil)
	if w := h.do(r); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "access_denied") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestLogout_NeedsTheSameOriginAndEndsTheSession(t *testing.T) {
	h := newHarness(t)
	h.accounts.Add(context.Background(), "dana", false)
	session := cookie(h.signIn(signIn{claims: dana("s")}), "souvenir-session")

	cross := httptest.NewRequest("POST", "/logout", nil)
	cross.AddCookie(session)
	cross.Header.Set("Origin", "https://evil.test")
	if w := h.do(cross); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site logout answered %d", w.Code)
	}
	if !strings.Contains(h.home(session), "Signed in") {
		t.Fatal("a refused logout ended the session")
	}

	same := httptest.NewRequest("POST", "/logout", nil)
	same.AddCookie(session)
	same.Header.Set("Origin", "http://souvenir.test")
	if w := h.do(same); w.Code != http.StatusSeeOther {
		t.Fatalf("logout answered %d", w.Code)
	}
	if strings.Contains(h.home(session), "Signed in") {
		t.Fatal("the session outlived logout")
	}
}

func TestRequireUser_RunsAsTheSessionsUser(t *testing.T) {
	h := newHarness(t)
	h.accounts.Add(context.Background(), "dana", false)
	session := cookie(h.signIn(signIn{claims: dana("s")}), "souvenir-session")
	var seen string
	protected := h.server.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := users.FromContext(r.Context())
		seen = u.Name
	}))

	w := httptest.NewRecorder()
	protected.ServeHTTP(w, httptest.NewRequest("GET", "/chat", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" || seen != "" {
		t.Fatalf("anonymous GET: %d %q", w.Code, seen)
	}
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, httptest.NewRequest("POST", "/chat", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST: %d", w.Code)
	}

	r := httptest.NewRequest("GET", "/chat", nil)
	r.AddCookie(session)
	protected.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "dana" {
		t.Fatalf("ran as %q", seen)
	}
}

func TestCookies_UseTheHostPrefixOverHttps(t *testing.T) {
	s := New(config.WebConfig{BaseUrl: "https://llm.example.com"}, config.OidcConfig{}, nil, nil)
	w := httptest.NewRecorder()
	s.setCookie(w, "session", "v", time.Hour)
	c := w.Result().Cookies()[0]
	if c.Name != "__Host-souvenir-session" || !c.Secure || c.Path != "/" || c.Domain != "" {
		t.Fatalf("%+v", c)
	}
}
