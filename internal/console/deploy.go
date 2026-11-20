package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Deploy uploads a new build where no registry/pull exists:
//
//   - systemd: the body replaces the target's Binary (atomic temp file + rename,
//     so it works while the service runs; the old file is backed up first).
//   - compose: the body is an image archive (`docker save`, plain or gzip) that is
//     run through `docker load`. It is kept in ImageDir when that is set,
//     otherwise staged in a temp file and removed afterwards.
//
// With restart=1 the service is then restarted (systemd: restart; compose:
// up -d, which recreates containers whose image changed). That needs the
// restart/up permission too. Output streams back as plain text like /run.

const deployAction = "deploy"

var imageNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@+-]{0,200}$`)

// handleDeploy: POST /api/targets/{name}/deploy?[service=S][&name=file.tar][&restart=1]
// The body is the raw file (curl --data-binary @file). For compose, file=NAME
// (empty body) loads an archive already in ImageDir instead.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	t, g, ok := s.target(w, r)
	if !ok {
		return
	}
	acts := actionsFor(g, t)
	if !hasAction(acts, deployAction) {
		http.Error(w, "deploy is not set up or not permitted on this target", http.StatusForbidden)
		return
	}
	service, ok := s.checkService(w, r, t, g)
	if !ok {
		return
	}
	q := r.URL.Query()
	restartName := "restart"
	if t.Type == "compose" {
		restartName = "up"
	}
	restart := q.Get("restart") == "1"
	if restart && !hasAction(acts, restartName) {
		http.Error(w, "you may upload but not "+restartName+" this target; upload without restart", http.StatusForbidden)
		return
	}
	// file=NAME: load an archive already in ImageDir (copied there by scp/USB on
	// servers that cannot reach a registry, or uploaded earlier = rollback).
	existing := q.Get("file")
	if existing != "" {
		if t.Type != "compose" || t.ImageDir == "" {
			http.Error(w, "loading a saved image needs a compose target with an image folder", http.StatusBadRequest)
			return
		}
		if !imageNameRe.MatchString(existing) {
			http.Error(w, "bad file name", http.StatusBadRequest)
			return
		}
	}
	imgName := ""
	if t.Type == "compose" && existing == "" {
		imgName = q.Get("name")
		if imgName == "" {
			imgName = "image.tar"
		}
		if !imageNameRe.MatchString(imgName) {
			http.Error(w, "bad file name", http.StatusBadRequest)
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
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	var (
		dest, backup string
		n            int64
		sum          string
		err          error
		cleanup      func()
	)
	switch {
	case existing != "":
		dest, n, err = savedImage(t, existing)
	case t.Type == "systemd":
		dest, backup, n, sum, err = s.putBinary(t, body)
	default:
		dest, n, sum, cleanup, err = s.putImage(t, imgName, body)
		if cleanup != nil {
			defer cleanup()
		}
	}
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "file is larger than maxUploadBytes ("+strconv.FormatInt(s.cfg.MaxUploadBytes, 10)+")", http.StatusRequestEntityTooLarge)
			return
		}
		fileError(w, err)
		return
	}
	log.Printf("audit: user=%s ip=%s target=%s deploy=%q saved=%v service=%q bytes=%d sha256=%s backup=%q restart=%v",
		who, r.RemoteAddr, t.Name, dest, existing != "", service, n, sum, backup, restart)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fw := &flushWriter{w: w, rc: http.NewResponseController(w)}
	if existing != "" {
		fmt.Fprintf(fw, "loading saved image %s (%s)\n", dest, fmtBytes(uint64(n)))
	} else {
		fmt.Fprintf(fw, "uploaded %s bytes · sha256 %s\n", strconv.FormatInt(n, 10), sum)
	}

	code := 0
	if t.Type == "systemd" {
		fmt.Fprintf(fw, "installed %s\n", dest)
		if backup != "" {
			fmt.Fprintf(fw, "previous version kept at %s\n", backup)
		}
		if restart {
			if isSelf(t.Unit) {
				fmt.Fprintf(fw, "\nThis is the deploy console itself: it restarts in 2 seconds with the new binary.\nThe page reconnects automatically; you stay signed in.\n")
				s.scheduleSelfRestart(t, who, r.RemoteAddr)
			} else if code = runStream(fw, t, []string{"systemctl", "restart", t.Unit}, 5*time.Minute); code == 0 {
				runStream(fw, t, []string{"systemctl", "status", "--no-pager", "--full", "-n", "20", t.Unit}, time.Minute)
			}
		}
	} else {
		code = runStream(fw, t, []string{"docker", "load", "-i", dest}, 30*time.Minute)
		if code == 0 && restart {
			a, _ := t.action("up")
			argv, _ := a.command(service)
			code = runStream(fw, t, argv, 30*time.Minute)
		}
	}
	if !restart && code == 0 {
		fmt.Fprintf(fw, "\nnot restarted: the new version runs after the next %s\n", restartName)
	}
	fmt.Fprintf(fw, "\n[exit %d]\n", code)
	log.Printf("audit: user=%s target=%s deploy done exit=%d", who, t.Name, code)
}

// savedImage resolves name in t.ImageDir. Symlinks are followed but the real
// file must still be directly in the folder: docker load runs as root/docker
// and must not be pointed at other host files.
func savedImage(t *Target, name string) (string, int64, error) {
	ops := t.fs()
	dir, err := ops.realpath(t.ImageDir)
	if err != nil {
		return "", 0, err
	}
	real, err := ops.realpath(filepath.Join(t.ImageDir, name))
	if err != nil {
		return "", 0, err
	}
	if filepath.Dir(real) != dir {
		return "", 0, errOutside
	}
	fi, err := ops.stat(real)
	if err != nil {
		return "", 0, err
	}
	if !fi.regular {
		return "", 0, errNotFile
	}
	return real, fi.size, nil
}

// handleImages: GET /api/targets/{name}/images -> archives in the image folder, newest first.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	t, g, ok := s.target(w, r)
	if !ok {
		return
	}
	if !hasAction(actionsFor(g, t), deployAction) {
		http.Error(w, "deploy is not permitted on this target", http.StatusForbidden)
		return
	}
	out := []entry{}
	if t.Type == "compose" && t.ImageDir != "" {
		all, err := t.fs().readDir(t.ImageDir)
		if err != nil {
			fileError(w, err)
			return
		}
		for _, e := range all {
			// plain files only: a symlink could point anywhere on the host
			if !e.Dir && !e.Link && imageNameRe.MatchString(e.Name) {
				out = append(out, e)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Mod.After(out[j].Mod) })
	}
	writeJSON(w, map[string]any{"dir": t.ImageDir, "files": out})
}

func hasAction(acts []Action, name string) bool {
	for _, a := range acts {
		if a.Name == name {
			return true
		}
	}
	return false
}

func hashing(r io.Reader) (io.Reader, func() string) {
	h := sha256.New()
	return io.TeeReader(r, h), func() string { return hex.EncodeToString(h.Sum(nil)) }
}

// putBinary atomically replaces t.Binary (following a symlink to the real file).
func (s *Server) putBinary(t *Target, body io.Reader) (dest, backup string, n int64, sum string, err error) {
	ops := t.fs()
	dest, err = ops.realpath(t.Binary)
	if err != nil {
		return "", "", 0, "", err
	}
	fi, err := ops.stat(dest)
	exists := err == nil
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", "", 0, "", err
	case exists && !fi.regular:
		return "", "", 0, "", errNotFile
	case exists:
		dir := filepath.Dir(dest)
		if backup, err = backupFile(ops, dir, dest, backupDirFor(s.cfg.BackupDir, t.Name, dir)); err != nil {
			return "", "", 0, "", err
		}
	}
	rd, done := hashing(body)
	n, err = ops.putFile(dest, rd, exists, true)
	return dest, backup, n, done(), err
}

// putImage stores the image archive in t.ImageDir, or a temp file when unset.
func (s *Server) putImage(t *Target, name string, body io.Reader) (dest string, n int64, sum string, cleanup func(), err error) {
	rd, done := hashing(body)
	if t.ImageDir == "" {
		f, err := os.CreateTemp("", "generic-deploy-image-*.tar")
		if err != nil {
			return "", 0, "", nil, err
		}
		cleanup = func() { os.Remove(f.Name()) }
		n, err = io.Copy(f, rd)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			// docker / sudo read it as another user (the docker CLI streams it to the daemon).
			err = os.Chmod(f.Name(), 0o644)
		}
		return f.Name(), n, done(), cleanup, err
	}
	ops := t.fs()
	dest = filepath.Join(t.ImageDir, name)
	fi, err := ops.stat(dest)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", 0, "", nil, err
	}
	if exists && !fi.regular {
		return "", 0, "", nil, errNotFile
	}
	n, err = ops.putFile(dest, rd, exists, false)
	return dest, n, done(), nil, err
}

// runStream runs argv for t, streaming its output to fw, and returns the exit code.
func runStream(fw io.Writer, t *Target, argv []string, timeout time.Duration) int {
	fmt.Fprintf(fw, "\n$ %s\n", strings.Join(argv, " "))
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := t.cmd(ctx, argv)
	cmd.Stdout, cmd.Stderr = fw, fw
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case ctx.Err() == context.DeadlineExceeded:
		fmt.Fprintf(fw, "[timed out after %s]\n", timeout)
		return -1
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		fmt.Fprintf(fw, "error: %v\n", err)
		return -1
	}
}

// ---- admin helper: find a unit's binary ----

var execPathRe = regexp.MustCompile(`path=([^ ;]+)`)

// handleDetectBinary: GET /api/admin/detect-binary?unit=U -> {"path": "...", "warning": "..."}
// Reads ExecStart from systemd so the admin does not have to look it up.
func (s *Server) handleDetectBinary(w http.ResponseWriter, r *http.Request) {
	unit := r.URL.Query().Get("unit")
	if unit == "" || strings.HasPrefix(unit, "-") || strings.ContainsAny(unit, " /\t") {
		http.Error(w, "bad unit name", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "ExecStart", "--value", "--", unit).Output()
	if err != nil {
		http.Error(w, "systemctl show failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	m := execPathRe.FindStringSubmatch(string(out))
	if m == nil {
		http.Error(w, "unit "+unit+" has no ExecStart (is it installed?)", http.StatusNotFound)
		return
	}
	p := m[1]
	warn := ""
	for _, sys := range []string{"/usr/bin/", "/bin/", "/usr/sbin/", "/sbin/", "/usr/lib/", "/snap/"} {
		if strings.HasPrefix(p, sys) {
			warn = "this looks like a system program (e.g. an interpreter or package-managed binary), not your app; set the path by hand if so"
		}
	}
	writeJSON(w, map[string]string{"path": p, "warning": warn})
}
