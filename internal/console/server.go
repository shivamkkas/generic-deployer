package console

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed web
var webFS embed.FS

const cookieName = "gd_session"

type ctxKey struct{}

type Server struct {
	cfg      *Config
	version  string
	store    *store
	users    *userStore
	sessions *sessions
}

func NewServer(cfg *Config, version string) (*Server, error) {
	sudoPassword = cfg.Sudo.Password
	if err := checkSudo(); err != nil {
		log.Printf("WARNING: %v; targets marked 'use sudo' will fail", err)
	} else if sudoPassword != "" {
		sudoOK = true
		log.Printf("sudo: password ok; available for targets marked 'use sudo'")
	}
	st, err := openStore(cfg.TargetsFile, cfg.Targets)
	if err != nil {
		return nil, err
	}
	us, err := openUserStore(cfg.UsersFile)
	if err != nil {
		return nil, err
	}
	if us.get(cfg.Auth.User) != nil {
		return nil, fmt.Errorf("%s: user %q clashes with the built-in admin in the main config", cfg.UsersFile, cfg.Auth.User)
	}
	return &Server{cfg: cfg, version: version, store: st, users: us, sessions: newSessions(filepath.Join(filepath.Dir(cfg.UsersFile), "sessions.json"))}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)

	mux.HandleFunc("GET /api/targets", s.handleTargets)
	mux.HandleFunc("GET /api/targets/{name}/state", s.handleState)
	mux.HandleFunc("GET /api/targets/{name}/services", s.handleServices)
	mux.HandleFunc("POST /api/targets/{name}/run/{action}", s.handleRun)
	mux.HandleFunc("GET /api/targets/{name}/files", s.handleList)
	mux.HandleFunc("GET /api/targets/{name}/file", s.handleRead)
	mux.HandleFunc("PUT /api/targets/{name}/file", s.handleWrite)
	mux.HandleFunc("POST /api/targets/{name}/upload", s.handleUpload)
	mux.HandleFunc("POST /api/targets/{name}/deploy", s.handleDeploy)
	mux.HandleFunc("GET /api/targets/{name}/images", s.handleImages)
	mux.HandleFunc("GET /api/targets/{name}/download", s.handleDownload)
	mux.HandleFunc("GET /api/targets/{name}/logs", s.handleLogsDownload)
	mux.HandleFunc("GET /api/targets/{name}/config", s.handleConfigDownload)

	// Admin only (checked in middleware).
	mux.HandleFunc("POST /api/admin/targets", s.handleTargetCreate)
	mux.HandleFunc("PUT /api/admin/targets/{name}", s.handleTargetUpdate)
	mux.HandleFunc("DELETE /api/admin/targets/{name}", s.handleTargetDelete)
	mux.HandleFunc("GET /api/admin/detect-binary", s.handleDetectBinary)
	mux.HandleFunc("GET /api/admin/users", s.handleUsers)
	mux.HandleFunc("POST /api/admin/users", s.handleUserCreate)
	mux.HandleFunc("PUT /api/admin/users/{name}", s.handleUserUpdate)
	mux.HandleFunc("DELETE /api/admin/users/{name}", s.handleUserDelete)
	return s.middleware(mux)
}

func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: action output streams for as long as the command runs.
	}
	if s.cfg.TLSCert != "" {
		return srv.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
	}
	return srv.ListenAndServe()
}

// ---------- auth ----------

// authenticate checks a name/password against the built-in admin and the user store.
func (s *Server) authenticate(name, pass string) *User {
	if name == s.cfg.Auth.User {
		if subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.Auth.Password)) == 1 {
			return s.superAdmin()
		}
		return nil
	}
	u := s.users.get(name)
	if u == nil || u.Disabled {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(pass)) // same cost as a real check
		return nil
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(pass)) != nil {
		return nil
	}
	return u
}

func (s *Server) superAdmin() *User { return &User{Name: s.cfg.Auth.User, Admin: true} }

// current resolves the caller from the session cookie or HTTP basic auth (for curl).
func (s *Server) current(r *http.Request) *User {
	if c, err := r.Cookie(cookieName); err == nil {
		switch name := s.sessions.user(c.Value); {
		case name == "":
		case name == s.cfg.Auth.User:
			return s.superAdmin()
		default:
			if u := s.users.get(name); u != nil && !u.Disabled {
				return u
			}
		}
	}
	if name, pass, ok := r.BasicAuth(); ok {
		return s.authenticate(name, pass)
	}
	return nil
}

func userFrom(r *http.Request) *User { u, _ := r.Context().Value(ctxKey{}).(*User); return u }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")

		// A custom header can only come from our own page (cross-origin needs a
		// preflight we never answer), so it blocks CSRF on anything that changes state.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Console") != "1" {
			http.Error(w, "missing X-Console header", http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/login" {
			next.ServeHTTP(w, r) // the UI's static files and the login call are public
			return
		}
		u := s.current(r)
		if u == nil {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/admin/") && !u.Admin {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u := s.authenticate(req.User, req.Password)
	if u == nil {
		log.Printf("auth: failed login user=%q ip=%s", req.User, r.RemoteAddr)
		time.Sleep(700 * time.Millisecond) // slow down guessing
		http.Error(w, "wrong user name or password", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.sessions.create(u.Name), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		MaxAge: int(sessionTTL / time.Second),
	})
	log.Printf("auth: login user=%s admin=%v ip=%s", u.Name, u.Admin, r.RemoteAddr)
	writeJSON(w, map[string]any{"name": u.Name, "admin": u.Admin})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	host, _ := os.Hostname()
	writeJSON(w, map[string]any{"name": u.Name, "admin": u.Admin, "hostname": host, "version": s.version,
		"sudo": u.Admin && sudoOK, "root": isRoot})
}

// ---------- permissions ----------

type rootView struct {
	Path string `json:"path"`
	Mode string `json:"mode"` // ro | rw
}

type targetView struct {
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Description string     `json:"description,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	Binary      string     `json:"binary,omitempty"`   // systemd: file the deploy upload replaces
	ImageDir    string     `json:"imageDir,omitempty"` // compose: folder of saved image archives
	Dir         string     `json:"dir,omitempty"`
	Problem     string     `json:"problem,omitempty"`
	Actions     []Action   `json:"actions"`
	Roots       []rootView `json:"roots"`
	Services    []string   `json:"services,omitempty"` // set when the user is limited to these compose services
	Config      *Target    `json:"config,omitempty"`   // full definition, admins only
}

// access returns the caller's grant for t. Admins get ok with a nil grant (= everything).
func access(u *User, t *Target) (*Grant, bool) {
	if u.Admin {
		return nil, true
	}
	g := u.grant(t.Name)
	return g, g != nil
}

func actionsFor(g *Grant, t *Target) []Action {
	all := t.Actions()
	if g == nil {
		return all
	}
	var out []Action
	for _, a := range all {
		if !slices.Contains(g.Actions, "*") && !slices.Contains(g.Actions, a.Name) {
			continue
		}
		if len(g.Services) > 0 && !a.Scopable {
			continue // service-limited users can't run project-wide commands (down, custom scripts…)
		}
		out = append(out, a)
	}
	return out
}

func rootsFor(g *Grant, t *Target) []rootView {
	var out []rootView
	if g == nil {
		mode := "rw"
		if t.ReadOnly {
			mode = "ro"
		}
		for _, r := range t.Roots {
			out = append(out, rootView{Path: r, Mode: mode})
		}
		return out
	}
	for _, p := range g.Paths {
		if !t.contains(p.Path) {
			continue // the target's directories changed since this grant was made
		}
		mode := "ro"
		if p.Mode == "rw" && !t.ReadOnly {
			mode = "rw"
		}
		out = append(out, rootView{Path: p.Path, Mode: mode})
	}
	return out
}

func (s *Server) view(u *User, t *Target) targetView {
	g, _ := access(u, t)
	v := targetView{
		Name: t.Name, Type: t.Type, Description: t.Description, Unit: t.Unit, Binary: t.Binary, ImageDir: t.ImageDir, Dir: t.Dir, Problem: t.Problem,
		Actions: actionsFor(g, t), Roots: rootsFor(g, t),
	}
	if v.Actions == nil {
		v.Actions = []Action{}
	}
	if v.Roots == nil {
		v.Roots = []rootView{}
	}
	if g != nil {
		v.Services = g.Services
	}
	if u.Admin {
		v.Config = t
	}
	return v
}

// target loads {name} and checks the caller may see it.
func (s *Server) target(w http.ResponseWriter, r *http.Request) (*Target, *Grant, bool) {
	t, _ := s.store.get(r.PathValue("name"))
	if t == nil {
		http.Error(w, "unknown target", http.StatusNotFound)
		return nil, nil, false
	}
	g, ok := access(userFrom(r), t)
	if !ok {
		http.Error(w, "unknown target", http.StatusNotFound) // don't reveal targets you can't use
		return nil, nil, false
	}
	return t, g, true
}

// ---------- targets: view & run ----------

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	out := []targetView{}
	for _, t := range s.store.list() {
		if _, ok := access(u, t); ok {
			out = append(out, s.view(u, t))
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if t, _, ok := s.target(w, r); ok {
		writeJSON(w, map[string]any{"state": t.State(r.Context()), "busy": s.busyFlag(t.Name).Load()})
	}
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	t, g, ok := s.target(w, r)
	if !ok {
		return
	}
	svcs, err := t.Services(r.Context())
	if err != nil {
		http.Error(w, "could not list services: "+err.Error(), http.StatusBadGateway)
		return
	}
	if g != nil {
		svcs = slices.DeleteFunc(svcs, func(sv Service) bool {
			return len(g.Services) > 0 && !slices.Contains(g.Services, sv.Name)
		})
		for i := range svcs {
			svcs[i].Mounts = nil // host paths are for the admin's grant editor
		}
	}
	if svcs == nil {
		svcs = []Service{}
	}
	writeJSON(w, svcs)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	t, g, ok := s.target(w, r)
	if !ok {
		return
	}
	var a Action
	found := false
	for _, x := range actionsFor(g, t) {
		if x.Name == r.PathValue("action") {
			a, found = x, true
		}
	}
	if !found {
		http.Error(w, "unknown or not permitted action", http.StatusForbidden)
		return
	}
	if a.Upload {
		http.Error(w, a.Label+" needs a file: POST it to /api/targets/"+t.Name+"/deploy", http.StatusBadRequest)
		return
	}
	service, ok := s.checkService(w, r, t, g)
	if !ok {
		return
	}
	argv, err := a.command(service)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if t.Type == "systemd" && isSelf(t.Unit) {
		if s.selfAction(w, r, t, a) {
			return
		}
	}

	busy := s.busyFlag(t.Name)
	if !busy.CompareAndSwap(false, true) {
		http.Error(w, "another action is already running on "+t.Name, http.StatusConflict)
		return
	}
	defer busy.Store(false)

	who := userFrom(r).Name
	log.Printf("audit: user=%s ip=%s target=%s action=%s service=%q argv=%q", who, r.RemoteAddr, t.Name, a.Name, service, argv)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fw := &flushWriter{w: w, rc: http.NewResponseController(w)}
	fmt.Fprintf(fw, "$ %s\n", strings.Join(argv, " "))

	// Deliberately not r.Context(): closing the browser tab must not kill a
	// half-finished pull/up. The command runs to completion or its timeout.
	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()
	cmd := t.cmd(ctx, argv)
	cmd.Stdout, cmd.Stderr = fw, fw
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	if a.fn != nil {
		err = a.fn(ctx, fw) // a non-exit error is shown as "error: …" with exit -1
	} else {
		err = cmd.Run()
	}
	took := time.Since(start).Round(100 * time.Millisecond)
	code := 0
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		code = -1
		fmt.Fprintf(fw, "error: %v\n", err)
	}
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(fw, "\n[timed out after %s]\n", a.timeout)
	}
	fmt.Fprintf(fw, "\n[exit %d after %s]\n", code, took)
	log.Printf("audit: user=%s target=%s action=%s service=%q exit=%d took=%s", who, t.Name, a.Name, service, code, took)
}

// checkService validates ?service= against the caller's grant: service-limited
// users must name one of their services; others may name any existing one.
func (s *Server) checkService(w http.ResponseWriter, r *http.Request, t *Target, g *Grant) (string, bool) {
	service := r.URL.Query().Get("service")
	if service != "" && (t.Type != "compose" || !serviceRe.MatchString(service)) {
		http.Error(w, "bad service name", http.StatusBadRequest)
		return "", false
	}
	if g != nil && len(g.Services) > 0 {
		if service == "" {
			http.Error(w, "choose one of your services: "+strings.Join(g.Services, ", "), http.StatusForbidden)
			return "", false
		}
		if !slices.Contains(g.Services, service) {
			http.Error(w, "you may not act on service "+service, http.StatusForbidden)
			return "", false
		}
	} else if service != "" {
		svcs, err := t.Services(r.Context())
		if err != nil || !slices.ContainsFunc(svcs, func(sv Service) bool { return sv.Name == service }) {
			http.Error(w, "unknown service "+service, http.StatusBadRequest)
			return "", false
		}
	}
	return service, true
}

// busyFlag never returns nil, so a target deleted mid-request is harmless.
func (s *Server) busyFlag(name string) *atomic.Bool {
	if _, b := s.store.get(name); b != nil {
		return b
	}
	return &atomic.Bool{}
}

// ---------- files ----------

// root picks the caller's ?root=N directory for t.
func (s *Server) root(w http.ResponseWriter, r *http.Request) (*Target, rootView, bool) {
	t, g, ok := s.target(w, r)
	if !ok {
		return nil, rootView{}, false
	}
	roots := rootsFor(g, t)
	i := 0
	if v := r.URL.Query().Get("root"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			n = -1
		}
		i = n
	}
	if len(roots) == 0 {
		fileError(w, errNoDir)
		return nil, rootView{}, false
	}
	if i < 0 || i >= len(roots) {
		fileError(w, errBadRoot)
		return nil, rootView{}, false
	}
	return t, roots[i], true
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if t, rt, ok := s.root(w, r); ok {
		entries, err := listDir(t.fs(), rt.Path, r.URL.Query().Get("path"))
		if err != nil {
			fileError(w, err)
			return
		}
		writeJSON(w, entries)
	}
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	if t, rt, ok := s.root(w, r); ok {
		b, err := readText(t.fs(), rt.Path, r.URL.Query().Get("path"), s.cfg.MaxFileBytes)
		if err != nil {
			fileError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	t, rt, ok := s.root(w, r)
	if !ok {
		return
	}
	if rt.Mode != "rw" {
		fileError(w, errReadOnly)
		return
	}
	q := r.URL.Query()
	path := q.Get("path")
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxFileBytes))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	if q.Get("force") != "1" {
		if err := checkContent(path, data); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
	}
	backup, err := writeText(t.fs(), rt.Path, path, data, q.Get("create") == "1", backupDirFor(s.cfg.BackupDir, t.Name, rt.Path))
	if err != nil {
		fileError(w, err)
		return
	}
	log.Printf("audit: user=%s ip=%s target=%s wrote=%q bytes=%d forced=%v backup=%q",
		userFrom(r).Name, r.RemoteAddr, t.Name, filepath.Join(rt.Path, filepath.Clean("/"+path)), len(data), q.Get("force") == "1", backup)
	writeJSON(w, map[string]any{"backup": backup, "bytes": len(data)})
}

// ---------- admin: targets ----------

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) decodeTarget(w http.ResponseWriter, r *http.Request) *Target {
	var t Target
	if !decodeJSON(w, r, &t) {
		return nil
	}
	t.Roots, t.Problem = nil, ""
	return &t
}

func (s *Server) handleTargetCreate(w http.ResponseWriter, r *http.Request) {
	t := s.decodeTarget(w, r)
	if t == nil {
		return
	}
	if err := s.store.put(t, true); err != nil {
		adminError(w, err)
		return
	}
	s.auditTarget(r, "created", t)
	writeJSON(w, s.view(userFrom(r), t))
}

func (s *Server) handleTargetUpdate(w http.ResponseWriter, r *http.Request) {
	t := s.decodeTarget(w, r)
	if t == nil {
		return
	}
	if t.Name != r.PathValue("name") {
		http.Error(w, "a target cannot be renamed; delete it and add a new one", http.StatusBadRequest)
		return
	}
	if err := s.store.put(t, false); err != nil {
		adminError(w, err)
		return
	}
	s.auditTarget(r, "updated", t)
	writeJSON(w, s.view(userFrom(r), t))
}

func (s *Server) handleTargetDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.remove(name); err != nil {
		adminError(w, err)
		return
	}
	if err := s.users.dropTarget(name); err != nil {
		log.Printf("warning: removing grants for deleted target %s: %v", name, err)
	}
	log.Printf("audit: user=%s ip=%s target=%s deleted", userFrom(r).Name, r.RemoteAddr, name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditTarget(r *http.Request, verb string, t *Target) {
	log.Printf("audit: user=%s ip=%s target=%s %s type=%s unit=%q binary=%q imageDir=%q dirs=%q sudo=%v commands=%q",
		userFrom(r).Name, r.RemoteAddr, t.Name, verb, t.Type, t.Unit, t.Binary, t.ImageDir, t.Roots, t.Sudo, t.Commands)
}

// ---------- admin: users ----------

type userReq struct {
	Name     string  `json:"name"`
	Password string  `json:"password"` // required on create; empty on update = keep
	Admin    bool    `json:"admin"`
	Disabled bool    `json:"disabled"`
	Grants   []Grant `json:"grants"`
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"builtinAdmin": s.cfg.Auth.User, "users": s.users.list()})
}

func (s *Server) buildUser(w http.ResponseWriter, req userReq, old *User) *User {
	u := &User{Name: req.Name, Admin: req.Admin, Disabled: req.Disabled, Grants: req.Grants}
	if u.Grants == nil {
		u.Grants = []Grant{}
	}
	if !nameRe.MatchString(u.Name) {
		http.Error(w, "user name: use letters, digits, _ . @ - (max 64)", http.StatusBadRequest)
		return nil
	}
	if u.Name == s.cfg.Auth.User {
		http.Error(w, "that name is the built-in admin from the main config", http.StatusConflict)
		return nil
	}
	switch {
	case req.Password != "":
		h, err := hashPassword(req.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return nil
		}
		u.PasswordHash = h
	case old != nil:
		u.PasswordHash = old.PasswordHash
	default:
		http.Error(w, "password is required for a new user", http.StatusBadRequest)
		return nil
	}
	if err := validateGrants(u, s.store); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil
	}
	return u
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req userReq
	if !decodeJSON(w, r, &req) {
		return
	}
	u := s.buildUser(w, req, nil)
	if u == nil {
		return
	}
	if err := s.users.put(u, true); err != nil {
		adminError(w, err)
		return
	}
	s.auditUser(r, "created", u)
	writeJSON(w, u)
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	var req userReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != r.PathValue("name") {
		http.Error(w, "a user cannot be renamed", http.StatusBadRequest)
		return
	}
	old := s.users.get(req.Name)
	if old == nil {
		adminError(w, errUserUnknown)
		return
	}
	u := s.buildUser(w, req, old)
	if u == nil {
		return
	}
	if err := s.users.put(u, false); err != nil {
		adminError(w, err)
		return
	}
	if req.Password != "" || u.Disabled {
		s.sessions.dropUser(u.Name) // force a fresh login
	}
	s.auditUser(r, "updated", u)
	writeJSON(w, u)
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.users.remove(name); err != nil {
		adminError(w, err)
		return
	}
	s.sessions.dropUser(name)
	log.Printf("audit: user=%s ip=%s deleted-user=%s", userFrom(r).Name, r.RemoteAddr, name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditUser(r *http.Request, verb string, u *User) {
	b, _ := json.Marshal(u.Grants)
	log.Printf("audit: user=%s ip=%s %s-user=%s admin=%v disabled=%v grants=%s",
		userFrom(r).Name, r.RemoteAddr, verb, u.Name, u.Admin, u.Disabled, b)
}

// ---------- helpers ----------

func adminError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, errTargetUnknown), errors.Is(err, errUserUnknown):
		code = http.StatusNotFound
	case errors.Is(err, errTargetExists), errors.Is(err, errTargetBusy), errors.Is(err, errUserExists):
		code = http.StatusConflict
	}
	http.Error(w, err.Error(), code)
}

func fileError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, errBadRoot):
		code = http.StatusNotFound
	case errors.Is(err, errOutside), errors.Is(err, errReadOnly), errors.Is(err, errNoDir), errors.Is(err, fs.ErrPermission):
		code = http.StatusForbidden
	case errors.Is(err, errExists):
		code = http.StatusConflict
	}
	msg := err.Error()
	if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
		msg = pe.Err.Error() // don't echo absolute server paths back to the browser
	}
	http.Error(w, msg, code)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// flushWriter streams command output to the browser. If the client goes away
// it swallows further writes so the command itself is not disturbed.
type flushWriter struct {
	mu   sync.Mutex
	w    io.Writer
	rc   *http.ResponseController
	dead bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dead {
		if _, err := f.w.Write(p); err != nil {
			f.dead = true
		} else {
			_ = f.rc.Flush()
		}
	}
	return len(p), nil
}
