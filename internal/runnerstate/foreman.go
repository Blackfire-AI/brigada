package runnerstate

import (
	"os"
	"path/filepath"
	"strings"
)

// LoadForeman reads the foreman init saved, as harness/model[@effort]. It is
// empty when init has not saved one yet.
func LoadForeman(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "foreman"))
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
	return os.Rename(f.Name(), filepath.Join(dir, "foreman"))
}
