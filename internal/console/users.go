package console

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// User is a console login created by an admin. Admins see and do everything;
// everyone else only gets what their Grants list.
type User struct {
	Name         string  `yaml:"name"               json:"name"`
	PasswordHash string  `yaml:"passwordHash"       json:"-"`
	Admin        bool    `yaml:"admin,omitempty"    json:"admin"`
	Disabled     bool    `yaml:"disabled,omitempty" json:"disabled"`
	Grants       []Grant `yaml:"grants,omitempty"   json:"grants"`
}

// Grant gives a user access to one target.
type Grant struct {
	Target   string      `yaml:"target"             json:"target"`
	Actions  []string    `yaml:"actions,omitempty"  json:"actions"`  // action names; "*" = all
	Services []string    `yaml:"services,omitempty" json:"services"` // compose services they may act on; empty = whole project
	Paths    []PathGrant `yaml:"paths,omitempty"    json:"paths"`    // directories they may open in the file browser
}

type PathGrant struct {
	Path string `yaml:"path" json:"path"`
	Mode string `yaml:"mode" json:"mode"` // ro | rw
}

func (u *User) grant(target string) *Grant {
	for i := range u.Grants {
		if u.Grants[i].Target == target {
			return &u.Grants[i]
		}
	}
	return nil
}

// ---------- store ----------

var (
	errUserExists  = errors.New("a user with that name already exists")
	errUserUnknown = errors.New("unknown user")
)

type userStore struct {
	mu    sync.RWMutex
	path  string
	order []string
	users map[string]*User
}

func openUserStore(path string) (*userStore, error) {
	us := &userStore{path: path, users: map[string]*User{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return us, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Users []*User `yaml:"users"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && len(bytes.TrimSpace(b)) > 0 {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, u := range f.Users {
		if u == nil || !nameRe.MatchString(u.Name) || us.users[u.Name] != nil {
			return nil, fmt.Errorf("%s: bad or duplicate user entry", path)
		}
		us.order = append(us.order, u.Name)
		us.users[u.Name] = u
	}
	return us, nil
}

// get returns a copy so callers never race with an admin edit.
func (us *userStore) get(name string) *User {
	us.mu.RLock()
	defer us.mu.RUnlock()
	if u := us.users[name]; u != nil {
		c := *u
		return &c
	}
	return nil
}

func (us *userStore) list() []*User {
	us.mu.RLock()
	defer us.mu.RUnlock()
	out := make([]*User, 0, len(us.order))
	for _, n := range us.order {
		c := *us.users[n]
		out = append(out, &c)
	}
	return out
}

func (us *userStore) put(u *User, create bool) error {
	us.mu.Lock()
	defer us.mu.Unlock()
	old := us.users[u.Name]
	switch {
	case create && old != nil:
		return errUserExists
	case !create && old == nil:
		return errUserUnknown
	}
	if create {
		us.order = append(us.order, u.Name)
	}
	us.users[u.Name] = u
	if err := us.save(); err != nil {
		if create {
			delete(us.users, u.Name)
			us.order = us.order[:len(us.order)-1]
		} else {
			us.users[u.Name] = old
		}
		return err
	}
	return nil
}

func (us *userStore) remove(name string) error {
	us.mu.Lock()
	defer us.mu.Unlock()
	old := us.users[name]
	if old == nil {
		return errUserUnknown
	}
	order := us.order
	delete(us.users, name)
	us.order = slices.DeleteFunc(slices.Clone(order), func(n string) bool { return n == name })
	if err := us.save(); err != nil {
		us.users[name], us.order = old, order
		return err
	}
	return nil
}

// dropTarget removes every grant for a deleted target.
func (us *userStore) dropTarget(target string) error {
	us.mu.Lock()
	defer us.mu.Unlock()
	changed := false
	for name, u := range us.users {
		// copy, never edit in place: get() hands out copies that share Grants
		ng := slices.DeleteFunc(slices.Clone(u.Grants), func(g Grant) bool { return g.Target == target })
		if len(ng) != len(u.Grants) {
			c := *u
			c.Grants = ng
			us.users[name] = &c
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return us.save()
}

func (us *userStore) save() error {
	f := struct {
		Users []*User `yaml:"users"`
	}{}
	for _, n := range us.order {
		f.Users = append(f.Users, us.users[n])
	}
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	b = append([]byte("# Managed by generic-deploy (edited from the UI). Manual edits apply on restart.\n"), b...)
	return writeFileAtomic(us.path, b)
}

// validateGrants checks a user's grants against the current targets.
func validateGrants(u *User, targets *store) error {
	seen := map[string]bool{}
	for i := range u.Grants {
		g := &u.Grants[i]
		t, _ := targets.get(g.Target)
		if t == nil {
			return fmt.Errorf("grant for unknown target %q", g.Target)
		}
		if seen[g.Target] {
			return fmt.Errorf("target %q granted twice", g.Target)
		}
		seen[g.Target] = true
		for _, a := range g.Actions {
			if _, ok := t.action(a); !ok && a != "*" {
				return fmt.Errorf("%s: unknown action %q", t.Name, a)
			}
		}
		if len(g.Services) > 0 && t.Type != "compose" {
			return fmt.Errorf("%s: services only apply to compose targets", t.Name)
		}
		for _, s := range g.Services {
			if !serviceRe.MatchString(s) {
				return fmt.Errorf("%s: bad service name %q", t.Name, s)
			}
		}
		for j := range g.Paths {
			p := &g.Paths[j]
			if p.Mode == "" {
				p.Mode = "ro"
			}
			if p.Mode != "ro" && p.Mode != "rw" {
				return fmt.Errorf("%s: path mode must be ro or rw, got %q", t.Name, p.Mode)
			}
			if !filepath.IsAbs(p.Path) {
				return fmt.Errorf("%s: path %q must be absolute", t.Name, p.Path)
			}
			p.Path = filepath.Clean(p.Path)
			if fi, err := t.fs().stat(p.Path); err != nil || !fi.dir {
				return fmt.Errorf("%s: path %q is not an existing directory", t.Name, p.Path)
			}
			if !t.contains(p.Path) {
				return fmt.Errorf("%s: path %q is not inside the target's directories (%v); add it to the target first", t.Name, p.Path, t.Roots)
			}
		}
	}
	return nil
}

// contains reports whether dir (after symlinks) lies within one of t's roots.
func (t *Target) contains(dir string) bool {
	ops := t.fs()
	real, err := ops.realpath(dir)
	if err != nil {
		return false
	}
	for _, r := range t.Roots {
		if rr, err := ops.realpath(r); err == nil && within(rr, real) {
			return true
		}
	}
	return false
}

func hashPassword(pw string) (string, error) {
	if len(pw) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// a real hash of a random string, so unknown users cost the same as wrong passwords
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)

// ---------- sessions ----------

const sessionTTL = 12 * time.Hour

// Sessions are kept in memory and mirrored to a 0600 file so a restart of
// the console doesn't sign everyone out. The file holds only SHA-256 hashes
// of the cookie tokens, never the tokens themselves.
type session struct {
	User    string    `json:"user"`
	Expires time.Time `json:"expires"`
}

type sessions struct {
	mu   sync.Mutex
	path string
	m    map[string]*session // key: hex(sha256(token))
}

func newSessions(path string) *sessions {
	s := &sessions{path: path, m: map[string]*session{}}
	if path == "" {
		return s
	}
	if b, err := os.ReadFile(path); err == nil {
		var m map[string]*session
		if json.Unmarshal(b, &m) == nil {
			now := time.Now()
			for k, v := range m {
				if v != nil && now.Before(v.Expires) {
					s.m[k] = v
				}
			}
		}
	}
	return s
}

func tokenKey(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// persist writes the session table; caller holds mu.
func (s *sessions) persist() {
	if s.path == "" {
		return
	}
	b, _ := json.Marshal(s.m)
	if err := writeFileAtomic(s.path, b); err != nil {
		log.Printf("warning: saving sessions: %v", err)
	}
}

func (s *sessions) create(user string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.m { // drop expired ones
		if now.After(v.Expires) {
			delete(s.m, k)
		}
	}
	s.m[tokenKey(tok)] = &session{User: user, Expires: now.Add(sessionTTL)}
	s.persist()
	return tok
}

// user returns the session's user name and slides its expiry (in memory).
func (s *sessions) user(tok string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := tokenKey(tok)
	ss := s.m[k]
	if ss == nil {
		return ""
	}
	if time.Now().After(ss.Expires) {
		delete(s.m, k)
		return ""
	}
	ss.Expires = time.Now().Add(sessionTTL)
	return ss.User
}

func (s *sessions) drop(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, tokenKey(tok))
	s.persist()
}

// dropUser logs a user out everywhere (password change, disable, delete).
func (s *sessions) dropUser(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if v.User == user {
			delete(s.m, k)
		}
	}
	s.persist()
}
