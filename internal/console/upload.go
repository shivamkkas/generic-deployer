package console

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Uploads replace files atomically: the new content is written to a temp file
// in the same directory and then renamed over the old one. Unlike the editor's
// in-place rewrite, this works on a binary a running service is executing
// (writing into it fails with "text file busy"); the running process keeps the
// old inode until it is restarted. The old file's mode and owner are kept.

// handleUpload: POST /api/targets/{name}/upload?root=N&path=dir/file[&overwrite=1][&exec=1]
// The body is the raw file content (not multipart), so curl --data-binary @file works.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	t, rt, ok := s.root(w, r)
	if !ok {
		return
	}
	if rt.Mode != "rw" {
		fileError(w, errReadOnly)
		return
	}
	q := r.URL.Query()
	ops := t.fs()
	root, p, err := resolve(ops, rt.Path, q.Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	if p == root {
		fileError(w, errIsDir)
		return
	}
	fi, err := ops.stat(p)
	exists := err == nil
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		fileError(w, err)
		return
	case exists && fi.dir:
		fileError(w, errIsDir)
		return
	case exists && !fi.regular:
		fileError(w, errNotFile)
		return
	case exists && q.Get("overwrite") != "1":
		fileError(w, errExists)
		return
	}

	backup := ""
	if exists {
		if backup, err = backupFile(ops, root, p, backupDirFor(s.cfg.BackupDir, t.Name, rt.Path)); err != nil {
			fileError(w, err)
			return
		}
	}
	h := sha256.New()
	body := io.TeeReader(http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes), h)
	n, err := ops.putFile(p, body, exists, q.Get("exec") == "1")
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "file is larger than maxUploadBytes ("+strconv.FormatInt(s.cfg.MaxUploadBytes, 10)+")", http.StatusRequestEntityTooLarge)
			return
		}
		fileError(w, err)
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	log.Printf("audit: user=%s ip=%s target=%s uploaded=%q bytes=%d sha256=%s replaced=%v backup=%q",
		userFrom(r).Name, r.RemoteAddr, t.Name, p, n, sum, exists, backup)
	writeJSON(w, map[string]any{"bytes": n, "sha256": sum, "replaced": exists, "backup": backup})
}

// backupFile streams the current p into backupDir (works for large binaries too).
func backupFile(ops fsOps, root, p, backupDir string) (string, error) {
	relp, _ := filepath.Rel(root, p)
	backup := filepath.Join(backupDir, relp+"."+time.Now().Format("20060102-150405.000"))
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	err = ops.cat(f, p)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(backup)
		return "", err
	}
	return backup, nil
}

func newMode(exec bool) os.FileMode {
	if exec {
		return 0o755
	}
	return 0o644
}

func randSuffix() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- direct ----

func (directFS) putFile(p string, r io.Reader, replace, exec bool) (int64, error) {
	dir, base := filepath.Split(p)
	tmp, err := os.CreateTemp(dir, "."+base+".upload-*")
	if err != nil {
		return 0, err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	n, err := io.Copy(tmp, r)
	if err != nil {
		return n, err
	}
	if err := tmp.Sync(); err != nil {
		return n, err
	}
	mode := newMode(exec)
	if replace {
		old, err := os.Stat(p)
		if err != nil {
			return n, err
		}
		mode = old.Mode().Perm() | old.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
		if st, ok := old.Sys().(*syscall.Stat_t); ok {
			// Only root can give a file away; as a normal user we own both anyway.
			if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil && isRoot {
				return n, err
			}
		}
	} else if _, err := os.Lstat(p); err == nil {
		return n, errExists // created by someone else while we were receiving
	}
	if err := tmp.Chmod(mode); err != nil {
		return n, err
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return n, err
	}
	ok = true
	return n, nil
}

// ---- sudo ----

// putFile stages the upload in a private temp file, copies it as root next to
// the destination, copies mode/owner from the old file, then renames it over.
func (s sudoFS) putFile(p string, r io.Reader, replace, exec bool) (int64, error) {
	stage, err := os.CreateTemp("", "generic-deploy-upload-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(stage.Name())
	n, err := io.Copy(stage, r)
	if err == nil {
		err = stage.Sync()
	}
	if cerr := stage.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	dir, base := filepath.Split(p)
	tmp := filepath.Join(dir, "."+base+".upload-"+randSuffix())
	if _, err := s.output("cp", "--no-preserve=all", "--", stage.Name(), tmp); err != nil {
		return n, err
	}
	fail := func(err error) (int64, error) {
		s.output("rm", "-f", "--", tmp)
		return n, err
	}
	if replace {
		if _, err := s.output("chmod", "--reference="+p, "--", tmp); err != nil {
			return fail(err)
		}
		if _, err := s.output("chown", "--reference="+p, "--", tmp); err != nil {
			return fail(err)
		}
	} else {
		if _, err := s.stat(p); err == nil {
			return fail(errExists)
		}
		if _, err := s.output("chmod", strconv.FormatUint(uint64(newMode(exec)), 8), "--", tmp); err != nil {
			return fail(err)
		}
	}
	if _, err := s.output("mv", "-f", "-T", "--", tmp, p); err != nil {
		return fail(err)
	}
	return n, nil
}
