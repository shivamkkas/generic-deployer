package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// sudoPassword is set once at startup (config sudo.password or the
// GENERIC_DEPLOY_SUDO_PASSWORD env var). Targets with Sudo: true run their
// commands and file access through `sudo -S`, with the password on stdin so
// it never shows up in the process list.
var sudoPassword string

// sudoOK records whether the startup check of sudoPassword succeeded.
var sudoOK bool

// useSudo: only when asked for, configured, and we aren't already root.
func (t *Target) useSudo() bool { return t.Sudo && sudoPassword != "" && !isRoot }

var isRoot = os.Geteuid() == 0

var baseEnv = []string{"TERM=dumb", "NO_COLOR=1", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0"}

// cmd builds the command for argv in the target's directory, wrapped in sudo
// when the target asks for it.
func (t *Target) cmd(ctx context.Context, argv []string) *exec.Cmd {
	dir := t.Dir
	if dir == "" {
		dir = "/"
	}
	if t.useSudo() {
		// `env -C` changes directory as root, so root-only (0700) dirs work too.
		full := append([]string{"sudo", "-S", "-k", "-p", "", "--", "env", "-C", dir}, baseEnv...)
		c := exec.CommandContext(ctx, full[0], append(full[1:], argv...)...)
		c.Dir = "/"
		c.Stdin = strings.NewReader(sudoPassword + "\n")
		return c
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = dir
	c.Env = append(os.Environ(), baseEnv...)
	return c
}

// checkSudo verifies the configured password works. Called at startup.
func checkSudo() error {
	if sudoPassword == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sudo", "-S", "-k", "-p", "", "--", "true")
	c.Stdin = strings.NewReader(sudoPassword + "\n")
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("sudo check failed: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------- file access ----------

type fileInfo struct {
	dir, regular bool
	size         int64
	mod          time.Time
}

// fsOps is how the file browser touches the disk: directly, or via sudo.
type fsOps interface {
	realpath(p string) (string, error) // resolve symlinks; the last component may not exist
	stat(p string) (fileInfo, error)
	readDir(p string) ([]entry, error)
	readFile(p string) ([]byte, error)
	writeFile(p string, data []byte, create bool) error               // existing files are rewritten in place
	putFile(p string, r io.Reader, replace, exec bool) (int64, error) // atomic replace (temp file + rename), see upload.go
	cat(w io.Writer, p string) error
	tarGz(w io.Writer, p string) error
}

func (t *Target) fs() fsOps {
	if t.useSudo() {
		return sudoFS{t}
	}
	return directFS{}
}

// ---- direct ----

type directFS struct{}

func (directFS) realpath(p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if errors.Is(err, fs.ErrNotExist) {
		parent, perr := filepath.EvalSymlinks(filepath.Dir(p))
		if perr != nil {
			return "", perr
		}
		return filepath.Join(parent, filepath.Base(p)), nil
	}
	return real, err
}

func (directFS) stat(p string) (fileInfo, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return fileInfo{}, err
	}
	return fileInfo{dir: fi.IsDir(), regular: fi.Mode().IsRegular(), size: fi.Size(), mod: fi.ModTime()}, nil
}

func (directFS) readDir(p string) ([]entry, error) {
	des, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(des))
	for _, de := range des {
		e := entry{Name: de.Name(), Link: de.Type()&fs.ModeSymlink != 0}
		if fi, err := os.Stat(filepath.Join(p, de.Name())); err == nil {
			e.Dir, e.Size, e.Mod = fi.IsDir(), fi.Size(), fi.ModTime()
		}
		out = append(out, e)
	}
	return out, nil
}

func (directFS) readFile(p string) ([]byte, error) { return os.ReadFile(p) }

func (directFS) writeFile(p string, data []byte, create bool) error {
	flag := os.O_WRONLY | os.O_TRUNC
	if create {
		flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(p, flag, 0o644)
	if err != nil {
		return err
	}
	return writeAndClose(f, data)
}

func (directFS) cat(w io.Writer, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func (directFS) tarGz(w io.Writer, p string) error { return tarGzDir(w, p) }

// ---- sudo ----

type sudoFS struct{ t *Target }

// run executes argv as root and returns stdout; stderr becomes the error.
func (s sudoFS) run(argv []string, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	full := append([]string{"sudo", "-S", "-k", "-p", "", "--"}, argv...)
	c := exec.CommandContext(ctx, full[0], full[1:]...)
	c.Dir = "/"
	c.Stdin = strings.NewReader(sudoPassword + "\n")
	var stderr bytes.Buffer
	c.Stdout, c.Stderr = stdout, &stderr
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		switch {
		case strings.Contains(msg, "No such file"):
			return fs.ErrNotExist
		case strings.Contains(msg, "Permission denied"):
			return fs.ErrPermission
		case strings.Contains(msg, "incorrect password"), strings.Contains(msg, "no password was provided"):
			return errors.New("sudo password from the config was rejected")
		case msg != "":
			return errors.New(msg)
		}
		return err
	}
	return nil
}

func (s sudoFS) output(argv ...string) ([]byte, error) {
	var b bytes.Buffer
	err := s.run(argv, &b)
	return b.Bytes(), err
}

func (s sudoFS) realpath(p string) (string, error) {
	out, err := s.output("realpath", "-m", "--", p)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (s sudoFS) stat(p string) (fileInfo, error) {
	out, err := s.output("stat", "-L", "-c", "%F|%s|%Y", "--", p)
	if err != nil {
		return fileInfo{}, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	if len(parts) != 3 {
		return fileInfo{}, fmt.Errorf("unexpected stat output %q", out)
	}
	size, _ := strconv.ParseInt(parts[1], 10, 64)
	sec, _ := strconv.ParseInt(parts[2], 10, 64)
	return fileInfo{
		dir:     parts[0] == "directory",
		regular: strings.Contains(parts[0], "regular"),
		size:    size, mod: time.Unix(sec, 0),
	}, nil
}

func (s sudoFS) readDir(p string) ([]entry, error) {
	// name \t type-after-symlinks \t own-type \t size \t mtime, NUL-separated (names may hold newlines)
	out, err := s.output("find", p, "-mindepth", "1", "-maxdepth", "1", "-printf", `%f\t%Y\t%y\t%s\t%T@\0`)
	if err != nil {
		return nil, err
	}
	var es []entry
	for _, rec := range bytes.Split(out, []byte{0}) {
		f := strings.Split(string(rec), "\t")
		if len(f) != 5 {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		secs, _ := strconv.ParseFloat(f[4], 64)
		es = append(es, entry{Name: f[0], Dir: f[1] == "d", Link: f[2] == "l", Size: size, Mod: time.Unix(int64(secs), 0)})
	}
	return es, nil
}

func (s sudoFS) readFile(p string) ([]byte, error) { return s.output("cat", "--", p) }

// writeFile stages the data in a private temp file, then `cp` copies it over
// the target. cp truncates and rewrites an existing file, so owner, mode and
// inode stay the same. (Data never goes through sudo's stdin, which carries
// the password.)
func (s sudoFS) writeFile(p string, data []byte, create bool) error {
	tmp, err := os.CreateTemp("", "generic-deploy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := writeAndClose(tmp, data); err != nil {
		return err
	}
	if create {
		if _, err := s.stat(p); err == nil {
			return errExists
		}
		if _, err := s.output("cp", "--no-preserve=all", "--", tmp.Name(), p); err != nil {
			return err
		}
		_, err := s.output("chmod", "0644", "--", p)
		return err
	}
	_, err = s.output("cp", "--no-preserve=all", "--", tmp.Name(), p)
	return err
}

func (s sudoFS) cat(w io.Writer, p string) error { return s.run([]string{"cat", "--", p}, w) }

func (s sudoFS) tarGz(w io.Writer, p string) error {
	return s.run([]string{"tar", "-czf", "-", "-C", filepath.Dir(p), "--", filepath.Base(p)}, w)
}
