package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"bandwagon/internal/kiwivm"
	"bandwagon/web"
)

// Server wires the data store, optional auth and HTTP routes together.
type Server struct {
	store   *Store
	auth    *authState
	shell   ShellConfig
	origins []string
	gate    *shellGate
	sshGate *sshGate
	mux     *http.ServeMux
}

// NewServer builds a Server from the loaded config. An empty Password disables
// auth entirely; Shell controls the Basic Shell, while the SSH terminal is
// enabled per instance by the presence of credentials.
func NewServer(store *Store, cfg *Config) *Server {
	s := &Server{
		store:   store,
		auth:    newAuth(cfg.Password),
		shell:   cfg.Shell,
		origins: cfg.AllowedOrigins,
		gate:    newShellGate(),
		sshGate: newSSHGate(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	mux := http.NewServeMux()

	// Static page (the login gate is rendered client-side; the page itself is
	// always served so users aren't locked out by a 401 on document load).
	mux.HandleFunc("GET /{$}", s.handleIndex)
	// Vendored front-end libraries (xterm.js and addons), served straight out of
	// the binary so a deployment never depends on a CDN. Cached briefly: the file
	// only changes when the binary does.
	mux.Handle("GET /vendor/", cacheFor(time.Hour,
		http.StripPrefix("/vendor/", http.FileServerFS(web.Vendor))))
	mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)

	mux.HandleFunc("GET /api/instances", s.requireAuth(s.handleList))
	mux.HandleFunc("GET /api/instances/{id}", s.requireAuth(s.handleOne))
	mux.HandleFunc("POST /api/instances/{id}/action", s.requireAuth(s.handleAction))
	mux.HandleFunc("POST /api/instances/{id}/shell", s.requireAuth(s.handleShell))
	// Real SSH terminal over WebSocket. Always registered: when an instance has
	// no credentials the handler explains that *inside* the socket, which is the
	// only channel a WebSocket client can actually read.
	mux.HandleFunc("GET /api/instances/{id}/ssh", s.requireAuth(s.handleSSH))

	s.mux = mux
}

// ListenAndServe starts the HTTP server, blocking until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("dashboard listening on %s (auth=%t shell=%t)", addr, s.auth.enabled, s.shell.Enabled)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(web.IndexHTML)
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       s.auth.enabled,
		"title":         snap.Title,
		"authed":        s.auth.checkRequest(r),
		"poll_seconds":  snap.PollSeconds,
		"shell_enabled": s.shell.Enabled,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if !s.auth.enabled {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if !s.auth.verify(body.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误"})
		return
	}
	token := s.auth.issue()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 30, // 30 days
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.auth.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Snapshot())
}

func (s *Server) handleOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap := s.store.Snapshot()
	for _, inst := range snap.Instances {
		if inst.ID == id {
			writeJSON(w, http.StatusOK, inst)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "instance not found"})
}

// handleAction runs a power action (start/stop/restart/kill) against an instance
// and then immediately refreshes that instance's snapshot.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	cfg, ok := s.store.configFor(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "instance not found"})
		return
	}
	if err := s.store.client.Action(cfg.VEID, cfg.APIKey, body.Action); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": kiwivm.FriendlyErr(err)})
		return
	}
	// Refresh this single instance so the UI reflects the change quickly.
	go s.store.refreshOne(cfg)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "action": body.Action})
}

// requireAuth wraps a handler; when auth is enabled it rejects requests without
// a valid session. API calls get 401 JSON; the browser login flow reacts to it.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.enabled || s.auth.checkRequest(r) {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
}

// cacheFor adds a Cache-Control header to static asset responses.
func cacheFor(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(d.Seconds())))
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

const sessionCookie = "bw_session"

// authState tracks an optional shared password and the set of valid session
// tokens issued by successful logins. Tokens are random and held in memory.
type authState struct {
	enabled  bool
	password string
	mu       sync.Mutex
	sessions map[string]struct{}
}

func newAuth(password string) *authState {
	return &authState{
		enabled:  password != "",
		password: password,
		sessions: make(map[string]struct{}),
	}
}

func (a *authState) verify(password string) bool {
	return constantTimeEq(password, a.password)
}

func (a *authState) issue() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	a.mu.Lock()
	a.sessions[token] = struct{}{}
	a.mu.Unlock()
	return token
}

func (a *authState) revoke(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

func (a *authState) checkRequest(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		_, ok := a.sessions[c.Value]
		a.mu.Unlock()
		if ok {
			return true
		}
	}
	// Allow API clients to pass a bearer token (the same session token).
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok := strings.TrimPrefix(h, "Bearer ")
		a.mu.Lock()
		_, ok := a.sessions[tok]
		a.mu.Unlock()
		return ok
	}
	return false
}

// constantTimeEq does a string compare that does not short-circuit, to avoid
// leaking the password length/contents via timing.
func constantTimeEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
