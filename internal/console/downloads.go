package console

import (
	"context"
	"fmt"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Downloads are plain GETs (so an <a download> link works with the session
// cookie) and never change anything on the host.

func attachment(w http.ResponseWriter, name, ctype string) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
}

func stamp() string { return time.Now().Format("20060102-150405") }

// handleDownload sends one file as-is (any size, binary too) or a folder as .tar.gz.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	t, rt, ok := s.root(w, r)
	if !ok {
		return
	}
	ops := t.fs()
	root, p, err := resolve(ops, rt.Path, r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	fi, err := ops.stat(p)
	if err != nil {
		fileError(w, err)
		return
	}
	log.Printf("audit: user=%s ip=%s target=%s download=%q dir=%v", userFrom(r).Name, r.RemoteAddr, t.Name, p, fi.dir)

	if !fi.dir {
		if !fi.regular {
			fileError(w, errNotFile)
			return
		}
		attachment(w, filepath.Base(p), "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(fi.size, 10))
		if err := ops.cat(w, p); err != nil {
			log.Printf("download %s: %v", p, err)
		}
		return
	}
	base := filepath.Base(p)
	if p == root {
		base = t.Name + "-" + filepath.Base(root)
	}
	attachment(w, base+"-"+stamp()+".tar.gz", "application/gzip")
	if err := ops.tarGz(w, p); err != nil {
		log.Printf("download %s: %v", p, err)
	}
}

// logSince maps the UI's choices to compose / journalctl arguments.
var logSince = map[string][2]string{
	"1h":  {"1h", "1 hour ago"},
	"6h":  {"6h", "6 hours ago"},
	"24h": {"24h", "24 hours ago"},
	"7d":  {"168h", "7 days ago"},
	"all": {"", ""},
}

// handleLogsDownload streams the full logs to a file instead of the page.
func (s *Server) handleLogsDownload(w http.ResponseWriter, r *http.Request) {
	t, g, ok := s.target(w, r)
	if !ok {
		return
	}
	if !slices.ContainsFunc(actionsFor(g, t), func(a Action) bool { return a.Name == "logs" }) {
		http.Error(w, "you are not allowed to read logs on this target", http.StatusForbidden)
		return
	}
	service, ok := s.checkService(w, r, t, g)
	if !ok {
		return
	}
	since, ok := logSince[r.URL.Query().Get("since")]
	if !ok {
		since = logSince["24h"]
	}
	var argv []string
	switch t.Type {
	case "compose":
		argv = append(t.composeBase(), "logs", "--no-color", "--timestamps")
		if since[0] != "" {
			argv = append(argv, "--since", since[0])
		}
		if service != "" {
			argv = append(argv, service)
		}
	case "systemd":
		argv = []string{"journalctl", "-u", t.Unit, "--no-pager", "-o", "short-iso"}
		if since[1] != "" {
			argv = append(argv, "--since", since[1])
		}
	}
	name := t.Name
	if service != "" {
		name += "-" + service
	}
	log.Printf("audit: user=%s ip=%s target=%s download-logs service=%q since=%q", userFrom(r).Name, r.RemoteAddr, t.Name, service, r.URL.Query().Get("since"))
	attachment(w, name+"-logs-"+stamp()+".log", "text/plain; charset=utf-8")
	s.streamCommand(w, r, t, argv, 15*time.Minute)
}

// handleConfigDownload gives the effective configuration: the fully resolved
// compose file, or the unit file(s) systemd is using.
func (s *Server) handleConfigDownload(w http.ResponseWriter, r *http.Request) {
	t, g, ok := s.target(w, r)
	if !ok {
		return
	}
	need := map[string]string{"compose": "config", "systemd": "status"}[t.Type]
	if !slices.ContainsFunc(actionsFor(g, t), func(a Action) bool { return a.Name == need }) {
		http.Error(w, "you are not allowed to read this target's configuration", http.StatusForbidden)
		return
	}
	var argv []string
	ext := ".txt"
	switch t.Type {
	case "compose":
		argv, ext = append(t.composeBase(), "config"), ".yaml"
	case "systemd":
		argv, ext = []string{"systemctl", "cat", "--no-pager", t.Unit}, ".service.txt"
	}
	log.Printf("audit: user=%s ip=%s target=%s download-config", userFrom(r).Name, r.RemoteAddr, t.Name)
	attachment(w, t.Name+"-config-"+stamp()+ext, "text/plain; charset=utf-8")
	s.streamCommand(w, r, t, argv, 2*time.Minute)
}

// streamCommand pipes a read-only command into the response. Unlike actions,
// it stops if the client goes away.
func (s *Server) streamCommand(w http.ResponseWriter, r *http.Request, t *Target, argv []string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	cmd := t.cmd(ctx, argv)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil && r.Context().Err() == nil {
		fmt.Fprintf(w, "\n[%s: %v]\n", strings.Join(argv[:min(3, len(argv))], " "), err)
	}
}
