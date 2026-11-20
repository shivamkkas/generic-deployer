package console

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadCreateReplaceAndBackup(t *testing.T) {
	hs := newHarness(t)
	data := "\x7fELF new binary\x00\x01"

	// new file: created with the requested exec bit
	w := hs.admin("POST", "/api/targets/app/upload?path=bin/myapp&exec=1", data)
	if w.Code != http.StatusNotFound { // bin/ doesn't exist yet
		t.Fatalf("upload into missing dir = %d %s", w.Code, w.Body)
	}
	os.MkdirAll(filepath.Join(hs.dir, "bin"), 0o755)
	w = hs.admin("POST", "/api/targets/app/upload?path=bin/myapp&exec=1", data)
	if w.Code != 200 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var res struct {
		Bytes    int64
		Sha256   string
		Replaced bool
		Backup   string
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	sum := sha256.Sum256([]byte(data))
	if res.Bytes != int64(len(data)) || res.Sha256 != hex.EncodeToString(sum[:]) || res.Replaced {
		t.Errorf("create result = %+v", res)
	}
	p := filepath.Join(hs.dir, "bin", "myapp")
	if fi, _ := os.Stat(p); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("new file mode = %v", fi)
	}

	// existing file without overwrite=1 is refused
	if c := hs.admin("POST", "/api/targets/app/upload?path=bin/myapp", "x").Code; c != http.StatusConflict {
		t.Errorf("replace without overwrite = %d", c)
	}

	// replace keeps the mode and backs up the old content
	os.Chmod(p, 0o750)
	w = hs.admin("POST", "/api/targets/app/upload?path=bin/myapp&overwrite=1", "v2")
	if w.Code != 200 {
		t.Fatalf("replace = %d %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if b, _ := os.ReadFile(p); string(b) != "v2" {
		t.Errorf("content after replace = %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o750 {
		t.Errorf("mode after replace = %v", fi.Mode())
	}
	if b, _ := os.ReadFile(res.Backup); string(b) != data || !res.Replaced {
		t.Errorf("backup %q = %q", res.Backup, b)
	}
	// no temp files left behind
	if es, _ := os.ReadDir(filepath.Join(hs.dir, "bin")); len(es) != 1 {
		t.Errorf("bin/ has %d entries", len(es))
	}

	// can't escape the root or replace a directory
	if c := hs.admin("POST", "/api/targets/app/upload?path=../../tmp/x", "x").Code; c == 200 {
		t.Error("upload escaped the root")
	}
	if c := hs.admin("POST", "/api/targets/app/upload?path=data&overwrite=1", "x").Code; c == 200 {
		t.Error("upload replaced a directory")
	}
}

func TestUploadPermissions(t *testing.T) {
	hs := newHarness(t)
	hs.admin("POST", "/api/admin/users", `{"name":"ro","password":"secret-pw","grants":[{"target":"app","actions":["ps"],"paths":[{"path":"`+hs.dir+`","mode":"ro"}]}]}`)
	if c := hs.do("POST", "/api/targets/app/upload?path=x.bin", "ro", "secret-pw", "x").Code; c != http.StatusForbidden {
		t.Errorf("read-only user upload = %d", c)
	}
	if _, err := os.Stat(filepath.Join(hs.dir, "x.bin")); err == nil {
		t.Error("read-only user created a file")
	}
}

func TestUploadTooLarge(t *testing.T) {
	hs := newHarness(t)
	hs.admin("POST", "/api/targets/app/upload?path=small", "ok") // create
	hs.s.cfg.MaxUploadBytes = 4
	w := hs.admin("POST", "/api/targets/app/upload?path=small&overwrite=1", "way too big")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large = %d %s", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(hs.dir, "small")); string(b) != "ok" {
		t.Errorf("file changed after rejected upload: %q", b)
	}
}

// TestUploadReplacesRunningBinary is the systemd case: the service's binary is
// executing while it gets replaced.
func TestUploadReplacesRunningBinary(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	hs := newHarness(t)
	p := filepath.Join(hs.dir, "svc")
	src, _ := os.Open(sleep)
	dst, _ := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o755)
	io.Copy(dst, src)
	src.Close()
	dst.Close()
	cmd := exec.Command(p, "30")
	if err := cmd.Start(); err != nil {
		t.Skip("can't execute from temp dir:", err)
	}
	defer cmd.Process.Kill()

	// the editor's in-place write fails on a running binary…
	if err := (directFS{}).writeFile(p, []byte("x"), false); err == nil || !strings.Contains(err.Error(), "text file busy") {
		t.Logf("in-place write on running binary: %v", err)
	}
	// …the upload doesn't
	w := hs.admin("POST", "/api/targets/app/upload?path=svc&overwrite=1", "#!/bin/sh\necho v2\n")
	if w.Code != 200 {
		t.Fatalf("replace running binary = %d %s", w.Code, w.Body)
	}
	out, err := exec.Command(p).Output()
	if err != nil || string(out) != "v2\n" {
		t.Errorf("new binary output = %q %v", out, err)
	}
}
