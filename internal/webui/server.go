// Package webui serves the gd control panel: a single-page dashboard over the
// existing CLI internals (config, daemon, serve, doctor). Localhost-only by
// design; API calls require a per-user token (GD_UI_TOKEN overrides it).
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gd/internal/config"
	"gd/internal/daemon"
	"gd/internal/doctor"
	"gd/internal/rclone"
	"gd/internal/serve"
	"gd/internal/version"
)

// DefaultPort is the port the UI listens on when --port is not given.
const DefaultPort = 5590

// Server is the gd control panel HTTP server.
type Server struct {
	Port     int
	Token    string // required for /api/*; GD_UI_TOKEN or ~/.gd/ui-token
	srv      *http.Server
	actionMu sync.Mutex // serializes mutating actions (add/mount/daemon/...)
}

// New builds a Server with an access token. GD_UI_TOKEN overrides the token;
// otherwise a per-user token is generated once and stored in ~/.gd/ui-token.
func New(port int) *Server {
	token := strings.TrimSpace(os.Getenv("GD_UI_TOKEN"))
	if token == "" {
		token = loadOrCreateToken()
	}
	return &Server{Port: port, Token: token}
}

// tokenFile returns the path of the generated UI token.
func tokenFile() (string, error) {
	dir, _, _, _, err := config.Paths()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ui-token"), nil
}

// loadOrCreateToken returns a stable token. If the file cannot be used, it
// falls back to an in-memory token so the API is never left unprotected.
func loadOrCreateToken() string {
	p, err := tokenFile()
	if err == nil {
		if data, err := os.ReadFile(p); err == nil {
			if token := strings.TrimSpace(string(data)); len(token) >= 16 {
				return token
			}
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	token := hex.EncodeToString(buf)
	if p != "" {
		_ = os.WriteFile(p, []byte(token), 0o600)
	}
	return token
}

// hostAllowed accepts only loopback host names. This blocks DNS rebinding.
func hostAllowed(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// originAllowed accepts an absent Origin, or one whose host is loopback.
func originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return hostAllowed(u.Host)
}

// actionOK is the JSON body for successful actions.
type actionOK struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// actionErr is the JSON body for failed actions.
type actionErr struct {
	Error string `json:"error"`
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// authMiddleware validates the loopback Host, enforces JSON POSTs, and gates
// API routes with the UI token. The dashboard itself is unauthenticated so the
// token can be delivered to the page; API calls from that page carry it.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host) {
			writeJSON(w, http.StatusForbidden, actionErr{Error: "forbidden host"})
			return
		}
		if r.Method == http.MethodPost {
			if !originAllowed(r.Header.Get("Origin")) {
				writeJSON(w, http.StatusForbidden, actionErr{Error: "forbidden origin"})
				return
			}
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				writeJSON(w, http.StatusUnsupportedMediaType, actionErr{Error: "Content-Type must be application/json"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			token := r.Header.Get("X-GD-Token")
			if s.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, actionErr{Error: "unauthorized: missing or invalid X-GD-Token"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Handler builds the full HTTP handler (exported for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/add", s.action(s.handleAdd))
	mux.HandleFunc("POST /api/mount", s.action(s.handleMount))
	mux.HandleFunc("POST /api/unmount", s.action(s.handleUnmount))
	mux.HandleFunc("POST /api/daemon", s.action(s.handleDaemon))
	mux.HandleFunc("POST /api/serve-s3", s.action(s.handleServeS3))
	mux.HandleFunc("POST /api/serve-webdav", s.action(s.handleServeWebDAV))
	mux.HandleFunc("POST /api/autostart", s.action(s.handleAutostart))
	mux.HandleFunc("POST /api/doctor", s.action(s.handleDoctor))
	mux.HandleFunc("POST /api/remove", s.action(s.handleRemove))
	mux.HandleFunc("/", s.handleIndex)
	return s.authMiddleware(mux)
}

// Run starts the (blocking) HTTP server on 127.0.0.1.
func (s *Server) Run() error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port))
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w (is another gd ui running? try --port)", addr, err)
	}
	return s.srv.Serve(ln)
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// indexHTMLBytes returns the dashboard HTML with the version and token
// placeholders replaced.
func indexHTMLBytes(token string) []byte {
	page := strings.ReplaceAll(indexHTML, versionPlaceholder, version.String())
	// JSON-encode the token so quotes or HTML metacharacters cannot break out
	// of the JavaScript string literal.
	if quoted, err := json.Marshal(token); err == nil {
		page = strings.ReplaceAll(page, `"`+tokenPlaceholder+`"`, string(quoted))
	}
	return []byte(page)
}

// handleHealthz is an unauthenticated identity probe for gd-ui and scripts.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"ok":      "true",
		"app":     "onlydrive",
		"version": version.String(),
	})
}

// handleIndex serves the single-page dashboard. Registered as catch-all, so
// wrong-method requests to API paths land here too: answer 405, not 404.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, actionErr{Error: "method not allowed for this endpoint"})
		return
	}
	// embedded logo (served from the binary, no external assets)
	if r.URL.Path == "/logo.png" {
		data, err := assets.ReadFile("assets/logo.png")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, actionErr{Error: "method not allowed"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTMLBytes(s.Token))
}

// action wraps a mutating handler with serialization and panic-safety.
func (s *Server) action(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.actionMu.Lock()
		defer s.actionMu.Unlock()
		defer func() {
			if rec := recover(); rec != nil {
				writeJSON(w, http.StatusInternalServerError, actionErr{Error: fmt.Sprintf("internal error: %v", rec)})
			}
		}()
		h(w, r)
	}
}

// handleState aggregates everything the dashboard shows.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
		return
	}
	st := statePayload{
		DaemonRunning: daemon.Running(),
		Accounts:      make([]accountView, 0, len(cfg.Accounts)),
		Mounts:        make([]config.Mount, 0, len(cfg.Mounts)),
		S3Running:     false,
	}
	s3Running, _, _, s3Addr := serve.S3Status()
	st.S3Running = s3Running
	st.S3Addr = s3Addr
	st.WebDAVRunning, st.WebDAVAddr = serve.WebDAVStatus()

	// OAuth client status for the accounts card: which client mints and
	// refreshes tokens. Only a masked hint of the client_id ever leaves.
	if id, _, _, err := config.ResolveOAuthClient(); err == nil && id != "" {
		st.OAuth.Own = true
		st.OAuth.ClientIDHint = config.MaskSecret(id)
	}

	if daemon.Running() {
		if m, err := manager(); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			if mounts, err := m.ListMounts(ctx); err == nil {
				for _, mm := range mounts {
					if pt, ok := mm["MountPoint"].(string); ok {
						st.ActiveMounts = append(st.ActiveMounts, pt)
					}
				}
			}
		}
	}
	for _, a := range cfg.Accounts {
		av := accountView{
			Name:    a.Name,
			Email:   a.Email,
			Remote:  a.Remote,
			Mounted: cfg.MountForAccount(a.Name) != nil,
		}
		if m, err := manager(); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			if about, err := m.About(ctx, a.Remote+":"); err == nil {
				av.Total, _ = about["total"].(float64)
				av.Used, _ = about["used"].(float64)
				av.QuotaOK = true
			}
		}
		st.Accounts = append(st.Accounts, av)
	}
	writeJSON(w, http.StatusOK, st)
}

// oauthView tells the dashboard which Google OAuth client is in effect.
type oauthView struct {
	Own          bool   `json:"own"`
	ClientIDHint string `json:"client_id_hint,omitempty"`
}

// statePayload is the GET /api/state response.
type statePayload struct {
	DaemonRunning bool           `json:"daemon_running"`
	Accounts      []accountView  `json:"accounts"`
	Mounts        []config.Mount `json:"mounts"`
	ActiveMounts  []string       `json:"active_mounts"`
	OAuth         oauthView      `json:"oauth"`
	S3Running     bool           `json:"s3_running"`
	S3Addr        string         `json:"s3_addr,omitempty"`
	WebDAVRunning bool           `json:"webdav_running"`
	WebDAVAddr    string         `json:"webdav_addr,omitempty"`
}

// accountView is one account row in the dashboard.
type accountView struct {
	Name    string  `json:"name"`
	Email   string  `json:"email"`
	Remote  string  `json:"remote"`
	Mounted bool    `json:"mounted"`
	QuotaOK bool    `json:"quota_ok"`
	Total   float64 `json:"total,omitempty"`
	Used    float64 `json:"used,omitempty"`
}

// Manager builds an authenticated rclone Manager (same as MCP server does).
// Exported for the tray status line, which reads pooled usage.
func Manager() (*rclone.Manager, error) { return manager() }

// manager builds an authenticated rclone Manager (same as MCP server does).
func manager() (*rclone.Manager, error) {
	m, err := rclone.New()
	if err != nil {
		return nil, err
	}
	pass, err := daemon.LoadPass()
	if err != nil {
		return nil, err
	}
	m.SetAuth("gd", pass)
	return m, nil
}

// handleAdd starts the OAuth flow for one more Google account.
func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	msg, err := addAccount()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionOK{OK: true, Message: msg})
}

// handleMount mounts one account (body: {"account":"acc1"} or first).
func (s *Server) handleMount(w http.ResponseWriter, r *http.Request) {
	acc, err := accountFromBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, actionErr{Error: err.Error()})
		return
	}
	if err := runMount(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionOK{OK: true, Message: "mounted " + acc})
}

// handleUnmount unmounts one account (body: {"account":"acc1"}).
func (s *Server) handleUnmount(w http.ResponseWriter, r *http.Request) {
	acc, err := accountFromBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, actionErr{Error: err.Error()})
		return
	}
	if err := runUnmount(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionOK{OK: true, Message: "unmounted " + acc})
}

// handleDaemon starts or stops the background daemon (body: {"action":"start"|"stop"}).
func (s *Server) handleDaemon(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch in.Action {
	case "start":
		st, err := daemon.Start()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, actionOK{OK: true, Message: "daemon: " + st})
	case "stop":
		if err := daemon.Stop(); err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, actionOK{OK: true, Message: "daemon stopped"})
	default:
		writeJSON(w, http.StatusBadRequest, actionErr{Error: `action must be "start" or "stop"`})
	}
}

// handleServeS3 starts or stops the S3 endpoint (body: {"action":"start"|"stop"}).
func (s *Server) handleServeS3(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch in.Action {
	case "start":
		if _, err := serve.EnsureUnion(); err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		acc, sec, msg, err := serve.S3Start("gd-union", 9000)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, actionOK{OK: true, Message: msg + "  access_key=" + acc + "  secret_key=" + sec})
	case "stop":
		if err := serve.S3Stop(); err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, actionOK{OK: true, Message: "S3 stopped"})
	default:
		writeJSON(w, http.StatusBadRequest, actionErr{Error: `action must be "start" or "stop"`})
	}
}

// handleServeWebDAV starts or stops the WebDAV endpoint (body: {"action":"start"|"stop"}).
func (s *Server) handleServeWebDAV(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch in.Action {
	case "start":
		if _, err := serve.EnsureUnion(); err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		msg, err := serve.WebDAVStart("gd-union", 9864)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, actionOK{OK: true, Message: msg})
	case "stop":
		if err := serve.WebDAVStop(); err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, actionOK{OK: true, Message: "WebDAV stopped"})
	default:
		writeJSON(w, http.StatusBadRequest, actionErr{Error: `action must be "start" or "stop"`})
	}
}

// handleAutostart turns logon autostart on or off (body: {"action":"on"|"off"}).
func (s *Server) handleAutostart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch in.Action {
	case "on":
		detail, err := serve.AutostartOn()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, actionOK{OK: true, Message: "autostart registered: " + detail})
	case "off":
		if err := serve.AutostartOff(); err != nil {
			writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, actionOK{OK: true, Message: "autostart removed"})
	default:
		writeJSON(w, http.StatusBadRequest, actionErr{Error: `action must be "on" or "off"`})
	}
}

// handleDoctor runs diagnostics (body: {"fix":true} to auto-fix).
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Fix bool `json:"fix"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	checks, err := doctor.Run(ctx, in.Fix)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "doctor finished",
		"checks":  checks,
	})
}

// handleRemove deletes an account (body: {"account":"acc1"}).
func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	acc, err := accountFromBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, actionErr{Error: err.Error()})
		return
	}
	if err := removeAccount(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, actionErr{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionOK{OK: true, Message: "removed " + acc})
}

// accountFromBody extracts {"account": "..."} allowing empty for "first".
func accountFromBody(r *http.Request) (string, error) {
	var in struct {
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return "", err
	}
	acc := strings.TrimSpace(in.Account)
	if acc == "" {
		cfg, err := config.Load()
		if err != nil {
			return "", err
		}
		if len(cfg.Accounts) == 0 {
			return "", fmt.Errorf("no accounts configured")
		}
		return cfg.Accounts[0].Name, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	if cfg.AccountByName(acc) == nil {
		return "", fmt.Errorf("account %q not found", acc)
	}
	return cfg.AccountByName(acc).Name, nil
}
