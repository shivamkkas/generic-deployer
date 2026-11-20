package console

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

var (
	errTargetExists  = errors.New("a target with that name already exists")
	errTargetUnknown = errors.New("unknown target")
	errTargetBusy    = errors.New("an action is running on this target; wait for it to finish")
)

// store holds the targets the admin manages from the UI and persists them to
// a YAML file (written atomically, mode 0600).
type store struct {
	mu      sync.RWMutex
	path    string
	order   []string
	targets map[string]*Target
	busy    map[string]*atomic.Bool
}

type targetsFile struct {
	Targets []*Target `yaml:"targets"`
}

// openStore loads path. If it does not exist yet it is created from seed
// (the targets: list in deploy.yaml, if any).
func openStore(path string, seed []*Target) (*store, error) {
	st := &store{path: path, targets: map[string]*Target{}, busy: map[string]*atomic.Bool{}}
	list := seed
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f targetsFile
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil && len(bytes.TrimSpace(b)) > 0 {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		list = f.Targets
		if len(seed) > 0 {
			log.Printf("note: %s exists, ignoring targets: in the main config (manage targets from the UI)", path)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, err
	}
	for i, t := range list {
		if t == nil {
			return nil, fmt.Errorf("%s: targets[%d] is empty", path, i)
		}
		if err := t.validate(false); err != nil {
			return nil, fmt.Errorf("%s: target %q: %w", path, t.Name, err)
		}
		if t.Problem != "" {
			log.Printf("warning: target %s: %s", t.Name, t.Problem)
		}
		if st.targets[t.Name] != nil {
			return nil, fmt.Errorf("%s: target %q defined twice", path, t.Name)
		}
		st.add(t)
	}
	if errors.Is(err, fs.ErrNotExist) && len(list) > 0 {
		if err := st.save(); err != nil {
			return nil, err
		}
		log.Printf("created %s from the targets in the main config", path)
	}
	return st, nil
}

func (st *store) add(t *Target) {
	st.order = append(st.order, t.Name)
	st.targets[t.Name] = t
	if st.busy[t.Name] == nil {
		st.busy[t.Name] = &atomic.Bool{}
	}
}

func (st *store) list() []*Target {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]*Target, 0, len(st.order))
	for _, n := range st.order {
		out = append(out, st.targets[n])
	}
	return out
}

func (st *store) get(name string) (*Target, *atomic.Bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.targets[name], st.busy[name]
}

// put creates (create=true) or replaces a target. Targets are never mutated in
// place: a running action keeps the *Target it started with.
func (st *store) put(t *Target, create bool) error {
	if err := t.validate(true); err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	old := st.targets[t.Name]
	switch {
	case create && old != nil:
		return errTargetExists
	case !create && old == nil:
		return errTargetUnknown
	}
	if create {
		st.add(t)
	} else {
		st.targets[t.Name] = t
	}
	if err := st.save(); err != nil {
		// roll back so memory matches disk
		if create {
			delete(st.targets, t.Name)
			st.order = st.order[:len(st.order)-1]
		} else {
			st.targets[t.Name] = old
		}
		return err
	}
	return nil
}

func (st *store) remove(name string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	old := st.targets[name]
	if old == nil {
		return errTargetUnknown
	}
	if st.busy[name].Load() {
		return errTargetBusy
	}
	order := st.order
	delete(st.targets, name)
	st.order = make([]string, 0, len(order))
	for _, n := range order {
		if n != name {
			st.order = append(st.order, n)
		}
	}
	if err := st.save(); err != nil {
		st.targets[name], st.order = old, order
		return err
	}
	delete(st.busy, name)
	return nil
}

// save writes the file; caller holds mu (or is still constructing st).
func (st *store) save() error {
	f := targetsFile{Targets: make([]*Target, 0, len(st.order))}
	for _, n := range st.order {
		f.Targets = append(f.Targets, st.targets[n])
	}
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	b = append([]byte("# Managed by generic-deploy (edited from the UI). Manual edits apply on restart.\n"), b...)
	return writeFileAtomic(st.path, b)
}

// writeFileAtomic replaces path with data (mode 0600) via a temp file + rename.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := writeAndClose(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
