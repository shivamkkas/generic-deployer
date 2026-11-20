package console

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// addTarget creates a target through the admin API and fails the test on error.
func (hs *harness) addTarget(body string) {
	hs.t.Helper()
	if w := hs.admin("POST", "/api/admin/targets", body); w.Code != 200 {
		hs.t.Fatalf("create target = %d %s", w.Code, w.Body)
	}
}

func TestDeploySystemdBinary(t *testing.T) {
	hs := newHarness(t)
	bin := t.TempDir()
	p := filepath.Join(bin, "svc")
	os.WriteFile(p, []byte("v1"), 0o750)

	// no binary configured: no deploy action, upload refused
	if c := hs.admin("POST", "/api/targets/other/deploy", "x").Code; c != http.StatusForbidden {
		t.Errorf("deploy without binary = %d", c)
	}
	// binary must be absolute and its folder must exist
	if c := hs.admin("POST", "/api/admin/targets", `{"name":"svc","type":"systemd","unit":"svc.service","binary":"svc"}`).Code; c != http.StatusBadRequest {
		t.Errorf("relative binary = %d", c)
	}
	if c := hs.admin("POST", "/api/admin/targets", `{"name":"svc","type":"systemd","unit":"svc.service","binary":"/nonexistent-dir/svc"}`).Code; c != http.StatusBadRequest {
		t.Errorf("binary in missing folder = %d", c)
	}
	hs.addTarget(`{"name":"svc","type":"systemd","unit":"svc.service","binary":"` + p + `"}`)

	// the deploy action is listed but cannot be run without a file
	if c := hs.admin("POST", "/api/targets/svc/run/deploy", "").Code; c != http.StatusBadRequest {
		t.Errorf("run/deploy = %d", c)
	}

	w := hs.admin("POST", "/api/targets/svc/deploy", "v2-binary")
	if w.Code != 200 {
		t.Fatalf("deploy = %d %s", w.Code, w.Body)
	}
	out := w.Body.String()
	if !strings.Contains(out, "installed "+p) || !strings.Contains(out, "[exit 0]") || !strings.Contains(out, "not restarted") {
		t.Errorf("deploy output = %q", out)
	}
	if b, _ := os.ReadFile(p); string(b) != "v2-binary" {
		t.Errorf("binary after deploy = %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o750 {
		t.Errorf("mode not kept: %v", fi.Mode())
	}
	backups, _ := filepath.Glob(filepath.Join(hs.s.cfg.BackupDir, "svc", "*", "svc.*"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); string(b) != "v1" {
		t.Errorf("backup content = %q", b)
	}

	// a symlinked binary: the real file is replaced, the link stays
	link := filepath.Join(bin, "current")
	os.Symlink(p, link)
	hs.admin("PUT", "/api/admin/targets/svc", `{"name":"svc","type":"systemd","unit":"svc.service","binary":"`+link+`"}`)
	if w := hs.admin("POST", "/api/targets/svc/deploy", "v3"); w.Code != 200 {
		t.Fatalf("deploy via link = %d %s", w.Code, w.Body)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced by a file")
	}
	if b, _ := os.ReadFile(p); string(b) != "v3" {
		t.Errorf("real file after deploy via link = %q", b)
	}

	// a user with deploy but not restart may upload, not restart
	hs.admin("POST", "/api/admin/users", `{"name":"ci","password":"secret-pw","grants":[{"target":"svc","actions":["deploy"]}]}`)
	if c := hs.do("POST", "/api/targets/svc/deploy?restart=1", "ci", "secret-pw", "v4").Code; c != http.StatusForbidden {
		t.Errorf("restart without grant = %d", c)
	}
	if c := hs.do("POST", "/api/targets/svc/deploy", "ci", "secret-pw", "v4").Code; c != 200 {
		t.Errorf("deploy with grant = %d", c)
	}
	// without the deploy grant: refused
	hs.admin("POST", "/api/admin/users", `{"name":"viewer","password":"secret-pw","grants":[{"target":"svc","actions":["status"]}]}`)
	if c := hs.do("POST", "/api/targets/svc/deploy", "viewer", "secret-pw", "v5").Code; c != http.StatusForbidden {
		t.Errorf("deploy without grant = %d", c)
	}
	if b, _ := os.ReadFile(p); string(b) != "v4" {
		t.Errorf("binary = %q", b)
	}
}

// fakeDocker puts a `docker` on PATH that records its arguments and copies
// the -i file to "loaded", so tests need no real Docker.
func fakeDocker(t *testing.T) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n" +
		"if [ \"$1\" = load ]; then cp \"$3\" " + filepath.Join(bin, "loaded") + "; echo 'Loaded image: app:latest'; fi\n"
	os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestDeployComposeImage(t *testing.T) {
	calls := fakeDocker(t)
	loaded := filepath.Join(filepath.Dir(calls), "loaded")
	hs := newHarness(t)

	// temp staging: loaded, then removed; up -d only with restart=1
	w := hs.admin("POST", "/api/targets/app/deploy?name=app.tar&restart=1", "IMAGE-TAR")
	if w.Code != 200 {
		t.Fatalf("deploy = %d %s", w.Code, w.Body)
	}
	out := w.Body.String()
	if !strings.Contains(out, "Loaded image: app:latest") || !strings.Contains(out, "[exit 0]") {
		t.Errorf("output = %q", out)
	}
	if b, _ := os.ReadFile(loaded); string(b) != "IMAGE-TAR" {
		t.Errorf("docker load got %q", b)
	}
	log, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "load -i ") || !strings.Contains(lines[1], "up -d --remove-orphans") {
		t.Errorf("docker calls = %q", lines)
	}
	if _, err := os.Stat(strings.TrimPrefix(lines[0], "load -i ")); err == nil {
		t.Error("staged image was not removed")
	}

	// bad file names are refused
	if c := hs.admin("POST", "/api/targets/app/deploy?name=../x.tar", "x").Code; c != http.StatusBadRequest {
		t.Errorf("bad name = %d", c)
	}

	// with an image folder the archive is kept there
	imgs := filepath.Join(hs.dir, "images")
	os.Mkdir(imgs, 0o755)
	hs.admin("PUT", "/api/admin/targets/app", `{"name":"app","type":"compose","dir":"`+hs.dir+`","imageDir":"`+imgs+`"}`)
	for _, v := range []string{"first", "second"} { // same name twice: replaced
		if w := hs.admin("POST", "/api/targets/app/deploy?name=app.tar", v); w.Code != 200 {
			t.Fatalf("deploy to image dir = %d %s", w.Code, w.Body)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(imgs, "app.tar")); string(b) != "second" {
		t.Errorf("kept archive = %q", b)
	}
	if c := hs.admin("PUT", "/api/admin/targets/app", `{"name":"app","type":"compose","dir":"`+hs.dir+`","imageDir":"/nonexistent"}`).Code; c != http.StatusBadRequest {
		t.Errorf("missing image dir = %d", c)
	}
	if c := hs.admin("PUT", "/api/admin/targets/app", `{"name":"app","type":"compose","dir":"`+hs.dir+`","binary":"/x"}`).Code; c != http.StatusBadRequest {
		t.Errorf("binary on compose = %d", c)
	}
}

func TestStatsAndStartActions(t *testing.T) {
	has := func(tg *Target, name string, scopable bool) bool {
		a, ok := tg.action(name)
		return ok && a.Scopable == scopable
	}
	c := &Target{Name: "c", Type: "compose", Dir: "/x"}
	if !has(c, "stats", true) || !has(c, "start", true) {
		t.Error("compose target lacks scopable stats/start")
	}
	s := &Target{Name: "s", Type: "systemd", Unit: "x.service"}
	if a, ok := s.action("stats"); !ok || a.fn == nil {
		t.Error("systemd target lacks stats")
	}
	if fmtBytes(462848) != "452.0 KiB" || fmtBytes(5<<30) != "5.0 GiB" || fmtBytes(12) != "12 B" {
		t.Errorf("fmtBytes: %s %s %s", fmtBytes(462848), fmtBytes(5<<30), fmtBytes(12))
	}
	if d := fmtDur(26*time.Hour + 3*time.Minute); d != "1d 2h 3m" {
		t.Errorf("fmtDur = %s", d)
	}
	if _, ok := num("[not set]"); ok {
		t.Error("num accepted [not set]")
	}
	if _, ok := num("18446744073709551615"); ok {
		t.Error("num accepted the unset max value")
	}
}

func TestDeployComposeSavedImage(t *testing.T) {
	calls := fakeDocker(t)
	loaded := filepath.Join(filepath.Dir(calls), "loaded")
	hs := newHarness(t)

	// needs an image folder
	if c := hs.admin("POST", "/api/targets/app/deploy?file=a.tar", "").Code; c != http.StatusBadRequest {
		t.Errorf("saved load without image folder = %d", c)
	}
	imgs := filepath.Join(hs.dir, "images")
	os.Mkdir(imgs, 0o755)
	os.WriteFile(filepath.Join(imgs, "app-1.0.tar.gz"), []byte("OLD"), 0o644)
	os.WriteFile(filepath.Join(imgs, "app-1.1.tar.gz"), []byte("NEW"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(imgs, "evil.tar"))
	os.Mkdir(filepath.Join(imgs, "sub"), 0o755)
	hs.admin("PUT", "/api/admin/targets/app", `{"name":"app","type":"compose","dir":"`+hs.dir+`","imageDir":"`+imgs+`"}`)

	w := hs.admin("GET", "/api/targets/app/images", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "app-1.0.tar.gz") || !strings.Contains(w.Body.String(), "app-1.1.tar.gz") ||
		strings.Contains(w.Body.String(), "evil") || strings.Contains(w.Body.String(), `"sub"`) {
		t.Errorf("images = %d %s", w.Code, w.Body)
	}

	// load a saved archive (roll back to 1.0) without uploading anything
	w = hs.admin("POST", "/api/targets/app/deploy?file=app-1.0.tar.gz", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "loading saved image") || !strings.Contains(w.Body.String(), "[exit 0]") {
		t.Fatalf("saved load = %d %s", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(loaded); string(b) != "OLD" {
		t.Errorf("docker loaded %q", b)
	}

	for _, bad := range []string{"../x.tar", "evil.tar", "sub", "missing.tar"} {
		if c := hs.admin("POST", "/api/targets/app/deploy?file="+bad, "").Code; c == 200 {
			t.Errorf("file=%s accepted", bad)
		}
	}

	// users need the deploy grant to list or load
	hs.admin("POST", "/api/admin/users", `{"name":"ops","password":"secret-pw","grants":[{"target":"app","actions":["ps"]}]}`)
	if c := hs.do("GET", "/api/targets/app/images", "ops", "secret-pw", "").Code; c != http.StatusForbidden {
		t.Errorf("images without grant = %d", c)
	}
	if c := hs.do("POST", "/api/targets/app/deploy?file=app-1.1.tar.gz", "ops", "secret-pw", "").Code; c != http.StatusForbidden {
		t.Errorf("load without grant = %d", c)
	}
}
