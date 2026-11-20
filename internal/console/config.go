package console

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole deploy.yaml. Auth is the built-in super-admin; other
// users are created by an admin in the UI and stored in UsersFile.
// Targets are managed by the admin from the
// UI and stored in TargetsFile; Targets here only seed that file the first time.
type Config struct {
	Listen       string    `yaml:"listen"`
	TLSCert      string    `yaml:"tlsCert"`
	TLSKey       string    `yaml:"tlsKey"`
	Auth         Auth      `yaml:"auth"`
	BackupDir    string    `yaml:"backupDir"`
	MaxFileBytes int64     `yaml:"maxFileBytes"`
	TargetsFile  string    `yaml:"targetsFile"`
	UsersFile    string    `yaml:"usersFile"`
	Sudo         SudoCfg   `yaml:"sudo"`
	Targets      []*Target `yaml:"targets"`
}

// SudoCfg lets the console (running as a normal user) act as root on targets
// the admin marks "use sudo". The password can also come from the
// GENERIC_DEPLOY_SUDO_PASSWORD environment variable.
type SudoCfg struct {
	Password string `yaml:"password"`
}

type Auth struct {
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

// Target is one managed thing: a docker compose project or a systemd unit.
type Target struct {
	Name         string              `yaml:"name"                   json:"name"`
	Type         string              `yaml:"type"                   json:"type"` // compose | systemd
	Description  string              `yaml:"description,omitempty"  json:"description,omitempty"`
	Dir          string              `yaml:"dir,omitempty"          json:"dir,omitempty"`  // compose project dir / service working dir; commands run here
	Dirs         []string            `yaml:"dirs,omitempty"         json:"dirs,omitempty"` // extra directories shown in the file browser
	Unit         string              `yaml:"unit,omitempty"         json:"unit,omitempty"` // systemd unit name
	ComposeFiles []string            `yaml:"composeFiles,omitempty" json:"composeFiles,omitempty"`
	Commands     map[string][]string `yaml:"commands,omitempty"     json:"commands,omitempty"` // extra buttons, run in Dir
	ReadOnly     bool                `yaml:"readOnly,omitempty"     json:"readOnly,omitempty"`
	Sudo         bool                `yaml:"sudo,omitempty"         json:"sudo,omitempty"` // run commands and file access as root via sudo

	Roots   []string `yaml:"-" json:"roots"`             // Dir + Dirs, deduped: what the file browser can open
	Problem string   `yaml:"-" json:"problem,omitempty"` // e.g. a configured dir that no longer exists
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,63}$`)

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.TargetsFile == "" {
		c.TargetsFile = filepath.Join(filepath.Dir(path), "targets.yaml")
	}
	if c.UsersFile == "" {
		c.UsersFile = filepath.Join(filepath.Dir(path), "users.yaml")
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8090"
	}
	if c.BackupDir == "" {
		c.BackupDir = "/var/lib/generic-deploy/backups"
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 1 << 20
	}
	if c.TargetsFile == "" {
		c.TargetsFile = "targets.yaml"
	}
	if c.UsersFile == "" {
		c.UsersFile = "users.yaml"
	}
	if c.Auth.User == "" || c.Auth.Password == "" || c.Auth.Password == "CHANGE-ME" {
		return errors.New("auth.user and auth.password must be set (and not CHANGE-ME)")
	}
	if v := os.Getenv("GENERIC_DEPLOY_SUDO_PASSWORD"); v != "" {
		c.Sudo.Password = v
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tlsCert and tlsKey must be set together")
	}
	return nil
}

// validate checks a target and fills Roots. Shape errors (bad name, unit,
// compose file, command) are always fatal. A configured directory that does
// not exist is fatal when strict (the admin is saving it); when loading the
// stored file it only sets Problem, so the target still shows up and can be fixed.
func (t *Target) validate(strict bool) error {
	t.Problem = ""
	if !nameRe.MatchString(t.Name) {
		return fmt.Errorf("name %q: use letters, digits, _ . @ - (max 64)", t.Name)
	}
	switch t.Type {
	case "compose":
		if t.Dir == "" {
			return errors.New("compose targets need dir (the compose project directory)")
		}
		if t.Unit != "" {
			return errors.New("unit only applies to systemd targets")
		}
		for _, f := range t.ComposeFiles {
			cf := filepath.Clean(f)
			if f == "" || filepath.IsAbs(cf) || cf == ".." || strings.HasPrefix(cf, "../") {
				return fmt.Errorf("compose file %q must be relative to dir", f)
			}
		}
	case "systemd":
		if t.Unit == "" || strings.HasPrefix(t.Unit, "-") || strings.ContainsAny(t.Unit, " /\t") {
			return fmt.Errorf("unit %q is not a valid unit name", t.Unit)
		}
		if len(t.ComposeFiles) > 0 {
			return errors.New("compose files only apply to compose targets")
		}
	default:
		return fmt.Errorf("type must be compose or systemd, got %q", t.Type)
	}
	for name, argv := range t.Commands {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("command name %q is invalid", name)
		}
		if len(argv) == 0 || argv[0] == "" {
			return fmt.Errorf("command %q has no program", name)
		}
	}

	var problems []string
	t.Roots = nil
	addRoot := func(d string) (string, error) {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("dir %q must be an absolute path", d)
		}
		d = filepath.Clean(d)
		if fi, err := t.fs().stat(d); err != nil || !fi.dir {
			if strict {
				return "", fmt.Errorf("dir %q is not an existing directory", d)
			}
			problems = append(problems, fmt.Sprintf("dir %s is missing", d))
		}
		if !slices.Contains(t.Roots, d) {
			t.Roots = append(t.Roots, d)
		}
		return d, nil
	}
	if t.Dir != "" {
		d, err := addRoot(t.Dir)
		if err != nil {
			return err
		}
		t.Dir = d
	}
	for i, raw := range t.Dirs {
		d, err := addRoot(raw)
		if err != nil {
			return err
		}
		t.Dirs[i] = d
	}
	t.Problem = strings.Join(problems, "; ")
	return nil
}
