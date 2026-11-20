package console

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	errNoDir    = errors.New("no directory is available here")
	errBadRoot  = errors.New("unknown directory index")
	errOutside  = errors.New("path is outside the allowed directory")
	errReadOnly = errors.New("this directory is read-only for you")
	errTooBig   = errors.New("file is larger than maxFileBytes")
	errBinary   = errors.New("file looks binary; only text files can be edited")
	errIsDir    = errors.New("path is a directory")
	errNotFile  = errors.New("not a regular file")
	errExists   = errors.New("file already exists")
)

// invalidContentError is returned when a .json/.yaml/.csv file doesn't parse.
type invalidContentError struct{ msg string }

func (e *invalidContentError) Error() string { return e.msg }

type entry struct {
	Name string    `json:"name"`
	Dir  bool      `json:"dir"`
	Link bool      `json:"link,omitempty"`
	Size int64     `json:"size"`
	Mod  time.Time `json:"mod"`
}

// resolve maps a client-relative path under rootDir (an absolute, already
// authorized directory) to an absolute path, following symlinks and refusing
// anything that ends up outside rootDir.
func resolve(ops fsOps, rootDir, rel string) (root, abs string, err error) {
	if rootDir == "" {
		return "", "", errNoDir
	}
	root, err = ops.realpath(rootDir)
	if err != nil {
		return "", "", err
	}
	// Clean("/"+rel) can never climb above "/", so the join stays under root.
	p := filepath.Join(root, filepath.Clean("/"+filepath.ToSlash(rel)))
	real, err := ops.realpath(p)
	if err != nil {
		return "", "", err
	}
	if !within(root, real) {
		return "", "", errOutside
	}
	return root, real, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func listDir(ops fsOps, rootDir, rel string) ([]entry, error) {
	_, p, err := resolve(ops, rootDir, rel)
	if err != nil {
		return nil, err
	}
	out, err := ops.readDir(p)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []entry{}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func readText(ops fsOps, rootDir, rel string, max int64) ([]byte, error) {
	_, p, err := resolve(ops, rootDir, rel)
	if err != nil {
		return nil, err
	}
	fi, err := ops.stat(p)
	if err != nil {
		return nil, err
	}
	if fi.dir {
		return nil, errIsDir
	}
	if !fi.regular {
		return nil, errNotFile
	}
	if fi.size > max {
		return nil, errTooBig
	}
	b, err := ops.readFile(p)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 {
		return nil, errBinary
	}
	return b, nil
}

// writeText saves data to rel under rootDir. An existing file is copied to
// backupDir first and then rewritten in place (same inode, so docker
// single-file bind mounts and the file's owner/mode are kept). create must be
// true to make a new file. backupDir should already be unique per target+root.
func writeText(ops fsOps, rootDir, rel string, data []byte, create bool, backupDir string) (backup string, err error) {
	root, p, err := resolve(ops, rootDir, rel)
	if err != nil {
		return "", err
	}
	fi, err := ops.stat(p)
	switch {
	case err == nil && create:
		return "", errExists
	case err == nil:
		if !fi.regular {
			return "", errNotFile
		}
		old, err := ops.readFile(p)
		if err != nil {
			return "", err
		}
		relp, _ := filepath.Rel(root, p)
		backup = filepath.Join(backupDir, relp+"."+time.Now().Format("20060102-150405.000"))
		if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(backup, old, 0o600); err != nil {
			return "", err
		}
		return backup, ops.writeFile(p, data, false)
	case errors.Is(err, fs.ErrNotExist) && create:
		return "", ops.writeFile(p, data, true)
	default:
		return "", err
	}
}

// backupDirFor keeps backups of different targets and roots apart.
func backupDirFor(base, target, root string) string {
	return filepath.Join(base, target, strings.ReplaceAll(strings.Trim(root, "/"), "/", "_"))
}

// tarGzDir streams dir as a .tar.gz (symlinks stored as links, never followed).
func tarGzDir(w io.Writer, dir string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip it, keep going
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		relp, _ := filepath.Rel(filepath.Dir(dir), path)
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			link, _ = os.Readlink(path)
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // sockets, devices, fifos
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(relp)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err // client went away
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			_, err = io.CopyN(tw, f, hdr.Size)
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// checkContent parses structured files by extension so a typo doesn't get
// deployed. Other extensions are accepted as plain text.
func checkContent(name string, data []byte) error {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json":
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			var se *json.SyntaxError
			if errors.As(err, &se) {
				line := 1 + bytes.Count(data[:min(int(se.Offset), len(data))], []byte("\n"))
				return &invalidContentError{fmt.Sprintf("invalid JSON at line %d: %v", line, err)}
			}
			return &invalidContentError{"invalid JSON: " + err.Error()}
		}
	case ".yaml", ".yml":
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var v any
			err := dec.Decode(&v)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return &invalidContentError{"invalid YAML: " + err.Error()}
			}
		}
	case ".csv", ".tsv":
		r := csv.NewReader(bytes.NewReader(data))
		if strings.EqualFold(filepath.Ext(name), ".tsv") {
			r.Comma = '\t'
		}
		if _, err := r.ReadAll(); err != nil {
			return &invalidContentError{"invalid CSV: " + err.Error()}
		}
	}
	return nil
}

func writeAndClose(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
