package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func newDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o640)
	os.MkdirAll(filepath.Join(dir, "data", "db"), 0o755)
	os.MkdirAll(filepath.Join(dir, "conf"), 0o755)
	return dir
}

func TestResolveStaysInside(t *testing.T) {
	dir := newDir(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600)
	os.Symlink(outside, filepath.Join(dir, "escape"))
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "secret-link"))

	for _, p := range []string{"escape/secret", "secret-link", "escape"} {
		if _, _, err := resolve(directFS{}, dir, p); !errors.Is(err, errOutside) {
			t.Errorf("resolve(%q) = %v, want errOutside", p, err)
		}
	}
	root, abs, err := resolve(directFS{}, dir, "../../etc/passwd")
	if err == nil && !strings.HasPrefix(abs, root) {
		t.Errorf("resolve escaped: %s", abs)
	}
	if _, abs, err := resolve(directFS{}, dir, "conf/../compose.yaml"); err != nil || filepath.Base(abs) != "compose.yaml" {
		t.Errorf("normal path: %v %s", err, abs)
	}
}

func TestWriteKeepsInodeModeAndBacksUp(t *testing.T) {
	dir := newDir(t)
	backups := t.TempDir()
	p := filepath.Join(dir, "compose.yaml")
	before, _ := os.Stat(p)

	backup, err := writeText(directFS{}, dir, "compose.yaml", []byte("services:\n  a: {}\n"), false, backups)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if !os.SameFile(before, after) {
		t.Error("file was replaced; want in-place write (same inode)")
	}
	if after.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", after.Mode().Perm())
	}
	if b, _ := os.ReadFile(backup); string(b) != "services: {}\n" {
		t.Errorf("backup content = %q", b)
	}
}

func TestWriteCreateRules(t *testing.T) {
	dir := newDir(t)
	if _, err := writeText(directFS{}, dir, "new.env", []byte("A=1\n"), false, t.TempDir()); err == nil {
		t.Error("writing a missing file without create should fail")
	}
	if _, err := writeText(directFS{}, dir, "new.env", []byte("A=1\n"), true, t.TempDir()); err != nil {
		t.Errorf("create: %v", err)
	}
	if _, err := writeText(directFS{}, dir, "new.env", []byte("A=2\n"), true, t.TempDir()); !errors.Is(err, errExists) {
		t.Errorf("create over existing = %v, want errExists", err)
	}
}

func TestReadRejectsBinary(t *testing.T) {
	dir := newDir(t)
	os.WriteFile(filepath.Join(dir, "bin"), []byte{0x7f, 'E', 'L', 'F', 0, 1}, 0o644)
	if _, err := readText(directFS{}, dir, "bin", 1<<20); !errors.Is(err, errBinary) {
		t.Errorf("err = %v, want errBinary", err)
	}
	if _, err := readText(directFS{}, dir, "compose.yaml", 3); !errors.Is(err, errTooBig) {
		t.Errorf("err = %v, want errTooBig", err)
	}
}

func TestCheckContent(t *testing.T) {
	cases := []struct {
		name, body string
		ok         bool
	}{
		{"a.json", `{"a": [1, 2]}`, true},
		{"a.json", "{\n  \"a\": 1,\n}", false},
		{"a.yaml", "a: 1\nb: [x, y]\n---\nc: 2\n", true},
		{"a.yml", "a: [1, 2\n", false},
		{"a.csv", "id,name\n1,x\n2,y\n", true},
		{"a.csv", "id,name\n1,x,extra\n", false},
		{"a.tsv", "id\tname\n1\tx\n", true},
		{"a.env", "anything goes {", true},
	}
	for _, c := range cases {
		err := checkContent(c.name, []byte(c.body))
		if (err == nil) != c.ok {
			t.Errorf("%s %q: err=%v, want ok=%v", c.name, c.body, err, c.ok)
		}
	}
	if err := checkContent("x.json", []byte("{\n\n  oops\n}")); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("want line number in JSON error, got %v", err)
	}
}

func TestActionsArgv(t *testing.T) {
	tg := &Target{Name: "app", Type: "compose", Dir: "/x", ComposeFiles: []string{"a.yml"},
		Commands: map[string][]string{"migrate": {"./migrate.sh", "--up"}}}
	a, _ := tg.action("recreate")
	if want := []string{"docker", "compose", "--ansi", "never", "-f", "a.yml", "up", "-d", "--force-recreate", "--remove-orphans"}; !slices.Equal(a.argv, want) {
		t.Errorf("project recreate = %q", a.argv)
	}
	argv, err := a.command("api")
	if want := []string{"docker", "compose", "--ansi", "never", "-f", "a.yml", "up", "-d", "--force-recreate", "--no-deps", "api"}; err != nil || !slices.Equal(argv, want) {
		t.Errorf("service recreate = %q %v", argv, err)
	}
	down, _ := tg.action("down")
	if _, err := down.command("api"); err == nil {
		t.Error("down must not be limited to a service")
	}
	if a, ok := tg.action("cmd:migrate"); !ok || !slices.Equal(a.argv, []string{"./migrate.sh", "--up"}) {
		t.Errorf("custom = %v %q", ok, a.argv)
	}
	sd := &Target{Name: "svc", Type: "systemd", Unit: "nginx.service"}
	if a, _ := sd.action("restart"); !slices.Equal(a.argv, []string{"systemctl", "restart", "nginx.service"}) {
		t.Errorf("systemd restart = %q", a.argv)
	}
}

func TestTargetValidation(t *testing.T) {
	dir := t.TempDir()
	good := func() *Target { return &Target{Name: "a", Type: "compose", Dir: dir} }
	if err := good().validate(true); err != nil {
		t.Fatalf("good: %v", err)
	}
	bad := map[string]func(*Target){
		"relative dir":     func(x *Target) { x.Dir = "rel" },
		"missing dir":      func(x *Target) { x.Dir = filepath.Join(dir, "nope") },
		"missing extra":    func(x *Target) { x.Dirs = []string{filepath.Join(dir, "nope")} },
		"bad type":         func(x *Target) { x.Type = "k8s" },
		"escaping compose": func(x *Target) { x.ComposeFiles = []string{"../x.yml"} },
		"unit flag":        func(x *Target) { x.Type = "systemd"; x.Unit = "--all" },
		"bad name":         func(x *Target) { x.Name = "a/b" },
		"empty command":    func(x *Target) { x.Commands = map[string][]string{"x": {}} },
	}
	for name, mutate := range bad {
		x := good()
		mutate(x)
		if x.validate(true) == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// lenient load: missing dir becomes a Problem, not an error
	x := good()
	x.Dirs = []string{filepath.Join(dir, "gone")}
	if err := x.validate(false); err != nil || x.Problem == "" || len(x.Roots) != 2 {
		t.Errorf("lenient: err=%v problem=%q roots=%v", err, x.Problem, x.Roots)
	}
}

// ---------- end-to-end over HTTP ----------

type harness struct {
	t   *testing.T
	h   http.Handler
	dir string
}

func newHarness(t *testing.T) *harness {
	dir := newDir(t)
	cfgDir := t.TempDir()
	cfg := &Config{Auth: Auth{User: "root-admin", Password: "pw"}, MaxFileBytes: 1 << 20,
		BackupDir: t.TempDir(), TargetsFile: filepath.Join(cfgDir, "targets.yaml"), UsersFile: filepath.Join(cfgDir, "users.yaml"),
		Targets: []*Target{{Name: "app", Type: "compose", Dir: dir}, {Name: "other", Type: "systemd", Unit: "x.service"}}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, h: s.Handler(), dir: dir}
}

func (hs *harness) do(method, url, user, pass, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	if user != "" {
		r.SetBasicAuth(user, pass)
	}
	r.Header.Set("X-Console", "1")
	w := httptest.NewRecorder()
	hs.h.ServeHTTP(w, r)
	return w
}

func (hs *harness) admin(method, url, body string) *httptest.ResponseRecorder {
	return hs.do(method, url, "root-admin", "pw", body)
}

func TestAuthCSRFAndAdminOnly(t *testing.T) {
	hs := newHarness(t)
	if c := hs.do("GET", "/api/targets", "", "", "").Code; c != http.StatusUnauthorized {
		t.Errorf("no auth = %d", c)
	}
	r := httptest.NewRequest("PUT", "/api/targets/app/file?path=compose.yaml", strings.NewReader("x"))
	r.SetBasicAuth("root-admin", "pw")
	w := httptest.NewRecorder()
	hs.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("PUT without X-Console = %d", w.Code)
	}
	if c := hs.admin("GET", "/", "").Code; c != http.StatusOK {
		t.Errorf("static UI = %d", c)
	}
	if c := hs.admin("POST", "/api/targets/app/run/bogus", "").Code; c != http.StatusForbidden {
		t.Errorf("unknown action = %d", c)
	}
	if c := hs.admin("GET", "/api/targets/app/file?path=../../../etc/passwd", "").Code; c == http.StatusOK {
		t.Error("traversal read succeeded")
	}

	// login sets a working session cookie
	w = hs.do("POST", "/api/login", "", "", `{"user":"root-admin","password":"pw"}`)
	if w.Code != 200 || len(w.Result().Cookies()) == 0 {
		t.Fatalf("login = %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(w.Result().Cookies()[0])
	w2 := httptest.NewRecorder()
	hs.h.ServeHTTP(w2, r)
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), `"admin":true`) {
		t.Errorf("session me = %d %s", w2.Code, w2.Body)
	}
}

func TestUserGrants(t *testing.T) {
	hs := newHarness(t)
	data := filepath.Join(hs.dir, "data")
	body := `{"name":"ravi","password":"secret-pw","grants":[{"target":"app","actions":["ps","restart","down"],"services":["api"],
		"paths":[{"path":"` + data + `","mode":"rw"},{"path":"` + filepath.Join(hs.dir, "conf") + `","mode":"ro"}]}]}`
	if w := hs.admin("POST", "/api/admin/users", body); w.Code != 200 {
		t.Fatalf("create user = %d %s", w.Code, w.Body)
	}
	u := func(method, url, body string) *httptest.ResponseRecorder {
		return hs.do(method, url, "ravi", "secret-pw", body)
	}

	// sees only the granted target, with only scopable granted actions
	var views []targetView
	json.Unmarshal(u("GET", "/api/targets", "").Body.Bytes(), &views)
	if len(views) != 1 || views[0].Name != "app" {
		t.Fatalf("views = %+v", views)
	}
	var names []string
	for _, a := range views[0].Actions {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"ps", "restart"}) { // down is project-wide: dropped for a service-limited user
		t.Errorf("actions = %v", names)
	}
	if views[0].Config != nil {
		t.Error("non-admin got the target config")
	}
	if len(views[0].Roots) != 2 || views[0].Roots[0].Mode != "rw" || views[0].Roots[1].Mode != "ro" {
		t.Errorf("roots = %+v", views[0].Roots)
	}

	// not admin, other targets invisible, ungranted/unscoped actions refused
	if c := u("GET", "/api/admin/users", "").Code; c != http.StatusForbidden {
		t.Errorf("admin api = %d", c)
	}
	if c := u("GET", "/api/targets/other/state", "").Code; c != http.StatusNotFound {
		t.Errorf("other target = %d", c)
	}
	if c := u("POST", "/api/targets/app/run/pull?service=api", "").Code; c != http.StatusForbidden {
		t.Errorf("ungranted action = %d", c)
	}
	if c := u("POST", "/api/targets/app/run/restart", "").Code; c != http.StatusForbidden {
		t.Errorf("restart without service = %d", c)
	}
	if c := u("POST", "/api/targets/app/run/restart?service=db", "").Code; c != http.StatusForbidden {
		t.Errorf("restart other service = %d", c)
	}

	// files: root 0 = data (rw), root 1 = conf (ro); project dir itself not reachable
	if c := u("PUT", "/api/targets/app/file?root=0&path=x.json&create=1", `{"ok":true}`).Code; c != 200 {
		t.Errorf("rw create = %d", c)
	}
	if c := u("PUT", "/api/targets/app/file?root=0&path=x.json", `{"broken":`).Code; c != http.StatusUnprocessableEntity {
		t.Errorf("invalid json = %d", c)
	}
	if c := u("PUT", "/api/targets/app/file?root=0&path=x.json&force=1", `{"broken":`).Code; c != 200 {
		t.Errorf("forced save = %d", c)
	}
	if c := u("PUT", "/api/targets/app/file?root=1&path=a.yaml&create=1", "a: 1").Code; c != http.StatusForbidden {
		t.Errorf("ro write = %d", c)
	}
	if c := u("GET", "/api/targets/app/file?root=0&path=../compose.yaml", "").Code; c == 200 {
		t.Error("escaped from granted data dir")
	}
	if c := u("GET", "/api/targets/app/files?root=2", "").Code; c != http.StatusNotFound {
		t.Errorf("bad root = %d", c)
	}

	// grant path outside the target's dirs is refused
	bad := `{"name":"ravi","grants":[{"target":"app","actions":["ps"],"paths":[{"path":"/etc","mode":"ro"}]}]}`
	if w := hs.admin("PUT", "/api/admin/users/ravi", bad); w.Code != http.StatusBadRequest {
		t.Errorf("outside path = %d %s", w.Code, w.Body)
	}

	// disabling locks the user out; deleting the target drops the grant
	hs.admin("PUT", "/api/admin/users/ravi", `{"name":"ravi","disabled":true,"grants":[]}`)
	if c := u("GET", "/api/targets", "").Code; c != http.StatusUnauthorized {
		t.Errorf("disabled user = %d", c)
	}
}

func TestTargetAdminCRUD(t *testing.T) {
	hs := newHarness(t)
	extra := t.TempDir()
	body := `{"name":"web","type":"compose","dir":"` + hs.dir + `","dirs":["` + extra + `"]}`
	if w := hs.admin("POST", "/api/admin/targets", body); w.Code != 200 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if c := hs.admin("POST", "/api/admin/targets", body).Code; c != http.StatusConflict {
		t.Errorf("duplicate = %d", c)
	}
	if c := hs.admin("POST", "/api/admin/targets", `{"name":"x","type":"compose","dir":"/nope/nope"}`).Code; c != http.StatusBadRequest {
		t.Errorf("missing dir = %d", c)
	}
	var views []targetView
	json.Unmarshal(hs.admin("GET", "/api/targets", "").Body.Bytes(), &views)
	if len(views) != 3 || len(views[2].Roots) != 2 {
		t.Fatalf("views = %+v", views)
	}
	if c := hs.admin("DELETE", "/api/admin/targets/web", "").Code; c != http.StatusNoContent {
		t.Errorf("delete = %d", c)
	}
}

func TestDownloads(t *testing.T) {
	hs := newHarness(t)
	os.WriteFile(filepath.Join(hs.dir, "data", "db", "blob.bin"), []byte{0, 1, 2, 3}, 0o644)

	w := hs.admin("GET", "/api/targets/app/download?path=data/db/blob.bin", "")
	if w.Code != 200 || w.Body.Len() != 4 || !strings.Contains(w.Header().Get("Content-Disposition"), "blob.bin") {
		t.Errorf("file download = %d len=%d cd=%q", w.Code, w.Body.Len(), w.Header().Get("Content-Disposition"))
	}
	w = hs.admin("GET", "/api/targets/app/download?path=data", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/gzip" || w.Body.Len() < 20 {
		t.Errorf("folder download = %d %q len=%d", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
	}
	if c := hs.admin("GET", "/api/targets/app/download?path=../../etc/passwd", "").Code; c == 200 {
		t.Error("download escaped the root")
	}

	// a user without the logs/config grant can't download them
	hs.admin("POST", "/api/admin/users", `{"name":"ops","password":"secret-pw","grants":[{"target":"app","actions":["ps"]}]}`)
	if c := hs.do("GET", "/api/targets/app/logs", "ops", "secret-pw", "").Code; c != http.StatusForbidden {
		t.Errorf("logs without grant = %d", c)
	}
	if c := hs.do("GET", "/api/targets/app/config", "ops", "secret-pw", "").Code; c != http.StatusForbidden {
		t.Errorf("config without grant = %d", c)
	}
	if c := hs.do("GET", "/api/targets/app/download?path=compose.yaml", "ops", "secret-pw", "").Code; c != http.StatusForbidden {
		t.Errorf("file download with no granted folders = %d", c)
	}
}

// TestSudoFS runs only when GD_TEST_SUDO_PASSWORD is set (needs a real sudo).
func TestSudoFS(t *testing.T) {
	pw := os.Getenv("GD_TEST_SUDO_PASSWORD")
	if pw == "" {
		t.Skip("set GD_TEST_SUDO_PASSWORD to run")
	}
	sudoPassword = pw
	defer func() { sudoPassword = "" }()
	dir := t.TempDir()
	tg := &Target{Name: "s", Type: "compose", Dir: dir, Sudo: true}
	ops := tg.fs()
	if _, ok := ops.(sudoFS); !ok {
		t.Fatal("expected sudoFS")
	}
	if _, err := writeText(ops, dir, "a.json", []byte(`{"x":1}`), true, t.TempDir()); err != nil {
		t.Fatalf("sudo create: %v", err)
	}
	if b, err := readText(ops, dir, "a.json", 1<<20); err != nil || string(b) != `{"x":1}` {
		t.Fatalf("sudo read = %q %v", b, err)
	}
	if es, err := listDir(ops, dir, ""); err != nil || len(es) != 1 || es[0].Name != "a.json" {
		t.Fatalf("sudo list = %+v %v", es, err)
	}
	out, err := tg.cmd(context.Background(), []string{"id", "-u"}).Output()
	if err != nil || strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("sudo cmd uid = %q %v", out, err)
	}
}
