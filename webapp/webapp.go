package webapp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/sessions"
	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/llm"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const callbackPath = "/auth/callback"

type Server struct {
	logger   slog.Logger
	cfg      config.WebConfig
	oidc     config.OidcConfig
	accounts *users.Store
	sessions *sessions.Store
	origin   string
	secure   bool
	// client talks to the provider.
	client *http.Client

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier

	app       App
	llm       llm.ChatClient
	turns     *turns
	modelList models
	location  *time.Location
	// base outlives requests: turns run on it.
	base context.Context
}

func New(cfg config.WebConfig, oc config.OidcConfig, accounts *users.Store, store *sessions.Store, app App) *Server {
	base, _ := url.Parse(cfg.BaseUrl)
	location := time.Local
	if tz := app.Config.Calendars.Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			location = loc
		}
	}
	return &Server{
		app:      app,
		llm:      *llm.NewLLMClient(app.Config),
		turns:    newTurns(),
		location: location,
		base:     context.Background(),
		logger:   *slog.Default().With("Component", "Web"),
		cfg:      cfg,
		oidc:     oc,
		accounts: accounts,
		sessions: store,
		origin:   base.Scheme + "://" + base.Host,
		secure:   base.Scheme == "https",
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("GET "+callbackPath, s.callback)
	mux.HandleFunc("POST /logout", s.logout)
	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /manifest.webmanifest", manifest)
	mux.Handle("GET /c/{id}", s.RequireUser(http.HandlerFunc(s.conversation)))
	mux.Handle("POST /messages", s.RequireUser(http.HandlerFunc(s.send)))
	mux.Handle("POST /c/{id}/title", s.RequireUser(http.HandlerFunc(s.rename)))
	mux.Handle("GET /settings", s.RequireUser(http.HandlerFunc(s.settingsHome)))
	mux.Handle("GET /settings/{tab}", s.RequireUser(http.HandlerFunc(s.settingsPage)))
	mux.Handle("POST /settings/tools", s.RequireUser(http.HandlerFunc(s.saveTools)))
	mux.Handle("POST /settings/calendars", s.RequireUser(http.HandlerFunc(s.saveCalendars)))
	mux.Handle("POST /settings/memories", s.RequireUser(http.HandlerFunc(s.addMemory)))
	mux.Handle("POST /settings/memories/{id}", s.RequireUser(http.HandlerFunc(s.updateMemory)))
	mux.Handle("POST /settings/memories/{id}/forget", s.RequireUser(http.HandlerFunc(s.forgetMemory)))
	mux.Handle("GET /turns/{id}/events", s.RequireUser(http.HandlerFunc(s.events)))
	mux.Handle("POST /turns/{id}/cancel", s.RequireUser(http.HandlerFunc(s.cancel)))
	return securityHeaders(s.sameOriginPosts(mux))
}

func (s *Server) sameOriginPosts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOrigin(r) {
			http.Error(w, "Cross-site request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	s.base = ctx
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errs := make(chan error, 1)
	go func() {
		s.logger.Info("Listening", "address", s.cfg.Listen, "url", s.cfg.BaseUrl)
		errs <- srv.ListenAndServe()
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func (s *Server) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		if !ok {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				http.Error(w, "Sign in first", http.StatusUnauthorized)
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(users.WithUser(r.Context(), u)))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; manifest-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cookieName(name string) string {
	if s.secure {
		return "__Host-souvenir-" + name
	}
	return "souvenir-" + name
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(name),
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	s.setCookie(w, name, "", -time.Second)
}

func (s *Server) currentUser(r *http.Request) (users.User, bool) {
	c, err := r.Cookie(s.cookieName("session"))
	if err != nil || c.Value == "" {
		return users.User{}, false
	}
	u, err := s.sessions.User(r.Context(), c.Value)
	if err != nil {
		if !errors.Is(err, sessions.ErrNotFound) {
			s.logger.Error("Could not read session", "error", err)
		}
		return users.User{}, false
	}
	return u, true
}

func (s *Server) sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		return origin == s.origin
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

func (s *Server) provider(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oauth != nil {
		return s.oauth, s.verifier, nil
	}
	ctx = oidc.ClientContext(context.WithoutCancel(ctx), s.client)
	p, err := oidc.NewProvider(ctx, s.oidc.Issuer)
	if err != nil {
		return nil, nil, err
	}
	s.oauth = &oauth2.Config{
		ClientID:     s.oidc.ClientId,
		ClientSecret: s.oidc.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  s.origin + callbackPath,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	s.verifier = p.Verifier(&oidc.Config{ClientID: s.oidc.ClientId})
	return s.oauth, s.verifier, nil
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		s.page(w, http.StatusOK, page{})
		return
	}
	s.newChat(w, r.WithContext(users.WithUser(r.Context(), u)))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	oauth, _, err := s.provider(r.Context())
	if err != nil {
		s.logger.Error("Could not reach the OIDC provider", "issuer", s.oidc.Issuer, "error", err)
		s.page(w, http.StatusServiceUnavailable, page{Message: "The sign-in provider cannot be reached right now. Try again shortly."})
		return
	}
	state, nonce, verifier := sessions.Token(), sessions.Token(), oauth2.GenerateVerifier()
	if err := s.sessions.BeginLogin(r.Context(), state, verifier, nonce); err != nil {
		s.logger.Error("Could not start sign-in", "error", err)
		s.page(w, http.StatusInternalServerError, page{Message: "Could not start signing in."})
		return
	}
	s.setCookie(w, "login", state, 10*time.Minute)
	http.Redirect(w, r, oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
}

type refusal string

func (r refusal) Error() string { return string(r) }

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	s.clearCookie(w, "login")
	if e := q.Get("error"); e != "" {
		s.logger.Warn("Sign-in failed at the provider", "error", e, "description", q.Get("error_description"))
		s.page(w, http.StatusBadRequest, page{Message: "Signing in did not complete (" + e + ").", Retry: true})
		return
	}
	state := q.Get("state")
	c, err := r.Cookie(s.cookieName("login"))
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		s.page(w, http.StatusBadRequest, page{Message: "This sign-in expired or was started in another browser.", Retry: true})
		return
	}
	pkce, nonce, err := s.sessions.TakeLogin(ctx, state)
	if err != nil {
		if !errors.Is(err, sessions.ErrNotFound) {
			s.logger.Error("Could not read sign-in", "error", err)
		}
		s.page(w, http.StatusBadRequest, page{Message: "This sign-in expired or was already used.", Retry: true})
		return
	}
	oauth, verifier, err := s.provider(ctx)
	if err != nil {
		s.logger.Error("Could not reach the OIDC provider", "issuer", s.oidc.Issuer, "error", err)
		s.page(w, http.StatusServiceUnavailable, page{Message: "The sign-in provider cannot be reached right now.", Retry: true})
		return
	}
	token, err := oauth.Exchange(oidc.ClientContext(ctx, s.client), q.Get("code"), oauth2.VerifierOption(pkce))
	if err != nil {
		s.logger.Error("Could not exchange the sign-in code", "error", err)
		s.page(w, http.StatusBadGateway, page{Message: "The sign-in provider did not accept the sign-in.", Retry: true})
		return
	}
	raw, _ := token.Extra("id_token").(string)
	idToken, err := verifier.Verify(ctx, raw)
	if err == nil && subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		err = errors.New("nonce does not match")
	}
	if err != nil {
		s.logger.Error("Rejected the ID token", "error", err)
		s.page(w, http.StatusBadGateway, page{Message: "The sign-in could not be verified.", Retry: true})
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		s.logger.Error("Could not read the ID token's claims", "error", err)
		s.page(w, http.StatusBadGateway, page{Message: "The sign-in could not be verified.", Retry: true})
		return
	}
	u, err := s.resolve(ctx, idToken.Issuer, idToken.Subject, claims)
	if why, ok := errors.AsType[refusal](err); ok {
		s.logger.Warn("Refused sign-in", "issuer", idToken.Issuer, "subject", idToken.Subject, "reason", string(why))
		s.page(w, http.StatusForbidden, page{Message: string(why)})
		return
	}
	if err != nil {
		s.logger.Error("Could not find the user for a sign-in", "error", err)
		s.page(w, http.StatusInternalServerError, page{Message: "Could not finish signing in."})
		return
	}
	ttl := time.Duration(s.cfg.SessionDays) * 24 * time.Hour
	session, err := s.sessions.Create(ctx, u.Id, ttl)
	if err != nil {
		s.logger.Error("Could not create a session", "error", err)
		s.page(w, http.StatusInternalServerError, page{Message: "Could not finish signing in."})
		return
	}
	s.setCookie(w, "session", session, ttl)
	s.logger.Info("Signed in", "user", u.Name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) resolve(ctx context.Context, issuer, subject string, claims map[string]any) (users.User, error) {
	u, err := s.accounts.ByIdentity(ctx, issuer, subject)
	if !errors.Is(err, users.ErrNotFound) {
		return u, err
	}
	claim := s.oidc.MatchClaim
	value, _ := claims[claim].(string)
	value = strings.TrimSpace(value)
	if value == "" {
		return u, refusal(fmt.Sprintf("Your sign-in does not say who you are (no %s), so it cannot be matched to an account.", claim))
	}
	if claim == "email" && claims["email_verified"] != true {
		return u, refusal("Your email address is not verified, so it cannot be matched to an account.")
	}
	for range 2 {
		u, err = s.accounts.ByName(ctx, value)
		if errors.Is(err, users.ErrNotFound) {
			u, err = s.accounts.AddLinked(ctx, value, issuer, subject)
			if errors.Is(err, users.ErrExists) {
				continue
			}
			if err == nil {
				s.logger.Info("Created a user for a sign-in", "user", u.Name, "issuer", issuer, "subject", subject, "claim", claim)
			}
			return u, err
		}
		if err != nil {
			return u, err
		}
		if err := s.accounts.LinkIdentity(ctx, u.Id, issuer, subject); err != nil {
			if errors.Is(err, users.ErrAlreadyLinked) {
				return u, refusal(fmt.Sprintf("The account %s is already linked to a different sign-in.", value))
			}
			return u, err
		}
		s.logger.Info("Linked a sign-in to a user", "user", u.Name, "issuer", issuer, "subject", subject, "claim", claim)
		return u, nil
	}
	return u, err
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookieName("session")); err == nil {
		if err := s.sessions.Delete(r.Context(), c.Value); err != nil {
			s.logger.Error("Could not end session", "error", err)
		}
	}
	s.clearCookie(w, "session")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type page struct {
	Message string
	Retry   bool
	Home    bool
}

func (s *Server) page(w http.ResponseWriter, status int, p page) {
	s.render(w, status, "page", p)
}
