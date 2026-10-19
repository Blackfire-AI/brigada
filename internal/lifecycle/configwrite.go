package lifecycle

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CheckConfigWritePath refuses symlinked config files. Parent directories may
// be symlinks, as when the user's config directory lives in a dotfiles repo.
// Init calls this before applying any part of its plan; the writer rechecks it.
func CheckConfigWritePath(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink at %s", path)
	}
	return nil
}

// SetConfigValues updates only the supplied keys, preserving other lines,
// comments and blanks. Missing keys share one init comment. Writes are atomic;
// new files are private, while existing files retain their permissions.
func SetConfigValues(path string, values map[string]string) (bool, error) {
	if err := CheckConfigWritePath(path); err != nil {
		return false, err
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	mode := os.FileMode(0600)
	if err == nil {
		info, err := os.Stat(path)
		if err != nil {
			return false, err
		}
		mode = info.Mode().Perm()
	}
	pending := make(map[string]string, len(values))
	for key, value := range values {
		pending[key] = value
	}
	lines := strings.Split(string(old), "\n")
	if len(old) == 0 {
		lines = nil
	} else if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	hasComment := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "# set by brigada init" {
			hasComment = true
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if next, supplied := values[key]; ok && supplied {
			delete(pending, key)
			if strings.TrimSpace(value) != next {
				lines[i] = key + " = " + next
			}
		}
	}
	if len(pending) > 0 {
		if !hasComment {
			lines = append(lines, "# set by brigada init")
		}
		keys := make([]string, 0, len(pending))
		for key := range pending {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			lines = append(lines, key+" = "+pending[key])
		}
	}
	next := []byte(strings.Join(lines, "\n"))
	if len(lines) > 0 {
		next = append(next, '\n')
	}
	// Preserve even a missing final newline when no key changed.
	if bytes.Equal(bytes.TrimSuffix(old, []byte("\n")),
		bytes.TrimSuffix(next, []byte("\n"))) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return false, err
	}
	if _, err := f.Write(next); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := CheckConfigWritePath(path); err != nil {
		return false, err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}
