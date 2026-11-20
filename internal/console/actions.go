package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Action is one fixed command the UI may run on a target. The UI only ever
// sends an action name (and optionally a compose service); argv comes from
// here, never from the client.
type Action struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Danger   bool   `json:"danger,omitempty"`   // UI asks for a second click
	Scopable bool   `json:"scopable,omitempty"` // can be limited to one compose service
	Upload   bool   `json:"upload,omitempty"`   // takes a file: POST /deploy, not /run
	argv     []string
	fn       func(ctx context.Context, w io.Writer) error // runs in Go instead of argv (argv is then only shown)
	scoped   []string                                     // argv used when a service is given; the service name is appended
	timeout  time.Duration
}

var serviceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (t *Target) composeBase() []string {
	base := []string{"docker", "compose", "--ansi", "never"}
	for _, f := range t.ComposeFiles {
		base = append(base, "-f", f)
	}
	return base
}

func (t *Target) Actions() []Action {
	var out []Action
	add := func(name, label string, danger bool, timeout time.Duration, argv ...string) {
		out = append(out, Action{Name: name, Label: label, Danger: danger, argv: argv, timeout: timeout})
	}
	// compose action: whole-project args, and per-service args (nil = project only)
	cadd := func(name, label string, danger bool, timeout time.Duration, project, perService []string) {
		c := func(args []string) []string { return append(slices.Clone(t.composeBase()), args...) }
		a := Action{Name: name, Label: label, Danger: danger, argv: c(project), timeout: timeout}
		if perService != nil {
			a.Scopable, a.scoped = true, c(perService)
		}
		out = append(out, a)
	}
	switch t.Type {
	case "compose":
		cadd("ps", "Status", false, time.Minute, []string{"ps", "-a"}, []string{"ps", "-a"})
		cadd("stats", "Stats", false, time.Minute, []string{"stats", "--no-stream"}, []string{"stats", "--no-stream"})
		cadd("start", "Start", false, 10*time.Minute, []string{"start"}, []string{"start"})
		cadd("pull", "Pull", false, 30*time.Minute, []string{"pull"}, []string{"pull"})
		cadd("up", "Up -d", false, 30*time.Minute, []string{"up", "-d", "--remove-orphans"}, []string{"up", "-d", "--no-deps"})
		cadd("recreate", "Force recreate", true, 30*time.Minute,
			[]string{"up", "-d", "--force-recreate", "--remove-orphans"}, []string{"up", "-d", "--force-recreate", "--no-deps"})
		cadd("restart", "Restart", true, 10*time.Minute, []string{"restart"}, []string{"restart"})
		cadd("stop", "Stop", true, 10*time.Minute, []string{"stop"}, []string{"stop"})
		cadd("down", "Down", true, 10*time.Minute, []string{"down", "--remove-orphans"}, nil)
		cadd("logs", "Logs (last 200)", false, time.Minute,
			[]string{"logs", "--no-color", "--timestamps", "--tail", "200"}, []string{"logs", "--no-color", "--timestamps", "--tail", "200"})
		cadd("config", "Validate", false, time.Minute, []string{"config", "--quiet"}, nil)
		out = append(out, Action{Name: deployAction, Label: "⇪ Load image", Danger: true, Scopable: true, Upload: true})
	case "systemd":
		u := t.Unit
		add("status", "Status", false, time.Minute, "systemctl", "status", "--no-pager", "--full", u)
		out = append(out, Action{Name: "stats", Label: "Stats", argv: []string{"systemctl", "show", u, "(resources)"}, timeout: time.Minute,
			fn: func(ctx context.Context, w io.Writer) error { return systemdStats(ctx, w, u) }})
		add("start", "Start", false, 5*time.Minute, "systemctl", "start", u)
		add("restart", "Restart", true, 5*time.Minute, "systemctl", "restart", u)
		add("stop", "Stop", true, 5*time.Minute, "systemctl", "stop", u)
		add("enable", "Enable", false, time.Minute, "systemctl", "enable", u)
		add("disable", "Disable", true, time.Minute, "systemctl", "disable", u)
		add("logs", "Logs (last 200)", false, time.Minute, "journalctl", "-u", u, "-n", "200", "--no-pager", "-o", "short-iso")
		add("daemon-reload", "Daemon reload", false, time.Minute, "systemctl", "daemon-reload")
		if t.Binary != "" {
			out = append(out, Action{Name: deployAction, Label: "⇪ Update binary", Danger: true, Upload: true})
		}
	}
	names := make([]string, 0, len(t.Commands))
	for n := range t.Commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		add("cmd:"+n, n, true, 30*time.Minute, t.Commands[n]...)
	}
	return out
}

func (t *Target) action(name string) (Action, bool) {
	for _, a := range t.Actions() {
		if a.Name == name {
			return a, true
		}
	}
	return Action{}, false
}

// command returns the argv for a, limited to service when one is given.
func (a Action) command(service string) ([]string, error) {
	if service == "" {
		return a.argv, nil
	}
	if !a.Scopable {
		return nil, fmt.Errorf("%s cannot be limited to one service", a.Label)
	}
	return append(slices.Clone(a.scoped), service), nil
}

// Service is one compose service and the host directories it bind-mounts.
type Service struct {
	Name   string   `json:"name"`
	Mounts []string `json:"mounts,omitempty"`
}

// Services lists the compose project's services (empty for systemd targets).
func (t *Target) Services(ctx context.Context) ([]Service, error) {
	if t.Type != "compose" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run := func(args ...string) ([]byte, error) {
		cmd := t.cmd(ctx, append(t.composeBase(), args...))
		out, err := cmd.Output()
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			err = fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return out, err
	}
	// --profile * lists services behind profiles too, so they can be picked and
	// acted on by name (compose activates a named service's profile itself).
	out, err := run("--profile", "*", "config", "--format", "json")
	if err != nil {
		out, err = run("config", "--format", "json")
	}
	if err == nil {
		var cfg struct {
			Services map[string]struct {
				Volumes []struct {
					Type   string `json:"type"`
					Source string `json:"source"`
				} `json:"volumes"`
			} `json:"services"`
		}
		if json.Unmarshal(out, &cfg) == nil && len(cfg.Services) > 0 {
			svcs := make([]Service, 0, len(cfg.Services))
			for name, s := range cfg.Services {
				sv := Service{Name: name}
				for _, v := range s.Volumes {
					if v.Type != "bind" || !filepath.IsAbs(v.Source) {
						continue
					}
					dir := filepath.Clean(v.Source)
					if fi, err := t.fs().stat(dir); err == nil && !fi.dir {
						dir = filepath.Dir(dir) // a single bind-mounted file: offer its folder
					}
					if !slices.Contains(sv.Mounts, dir) {
						sv.Mounts = append(sv.Mounts, dir)
					}
				}
				svcs = append(svcs, sv)
			}
			sort.Slice(svcs, func(i, j int) bool { return svcs[i].Name < svcs[j].Name })
			return svcs, nil
		}
	}
	// Older compose without --format json: names only.
	out, err = run("--profile", "*", "config", "--services")
	if err != nil {
		if out, err = run("config", "--services"); err != nil {
			return nil, err
		}
	}
	var svcs []Service
	for _, n := range strings.Fields(string(out)) {
		svcs = append(svcs, Service{Name: n})
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].Name < svcs[j].Name })
	return svcs, nil
}

// State is a one-line health summary for the sidebar.
func (t *Target) State(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch t.Type {
	case "systemd":
		out, _ := exec.CommandContext(ctx, "systemctl", "is-active", t.Unit).Output() // works without root
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	case "compose":
		out, err := t.cmd(ctx, append(t.composeBase(), "ps", "-a", "--format", "{{.State}}")).Output()
		if err != nil {
			return "unknown"
		}
		states := strings.Fields(string(out))
		if len(states) == 0 {
			return "no containers"
		}
		running := 0
		for _, s := range states {
			if s == "running" {
				running++
			}
		}
		return fmt.Sprintf("%d/%d running", running, len(states))
	}
	return "unknown"
}
