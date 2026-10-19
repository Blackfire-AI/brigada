package runnerstate

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// foremanFile holds the foreman init saved. Its first name, "foreman", is
// the foreman's scratch directory (runner.ForemanScratchDir), so a file
// left under that name is moved here by MigrateForeman.
const foremanFile = "foreman-choice"

// LoadForeman reads the foreman init saved, as harness/model[@effort]. It is
// empty when init has not saved one yet.
func LoadForeman(dir string) (string, error) {
	if err := MigrateForeman(dir); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, foremanFile))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// SaveForeman atomically replaces the private, durable foreman choice.
func SaveForeman(dir, foreman string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := MigrateForeman(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".foreman-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(foreman + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, foremanFile))
}

// MigrateForeman moves a foreman choice saved as the file "foreman" to its
// current name, freeing the path for the foreman's scratch directory. A
// directory there is the scratch directory and stays; a choice already
// saved under the current name wins over the old file.
//
// Foreman turns and attaches may migrate at once: a call that loses the
// race to another finds no old file left and succeeds.
func MigrateForeman(dir string) error {
	old := filepath.Join(dir, "foreman")
	if ok, err := oldForeman(old); !ok {
		return err
	}
	// A link, unlike a rename, never replaces a choice saved under the new
	// name, and never moves a scratch directory made since the check.
	err := os.Link(old, filepath.Join(dir, foremanFile))
	if os.IsExist(err) {
		// The new name may exist because another call migrated: remove
		// only what is still the old file.
		if ok, statErr := oldForeman(old); !ok {
			return statErr
		}
		err = nil
	}
	if err == nil {
		// Unlink, unlike os.Remove, never falls back to rmdir: another
		// call may have migrated already and a turn made the scratch
		// directory here, still empty.
		err = syscall.Unlink(old)
	}
	if err != nil {
		if ok, statErr := oldForeman(old); ok || statErr != nil {
			return err
		}
	}
	return nil
}

// oldForeman reports whether path is still a foreman choice saved under
// the old name: a regular file, not the scratch directory.
func oldForeman(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}
