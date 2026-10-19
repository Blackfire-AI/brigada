package runnerstate

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LoadParallel returns the saved init choice, or zero when none was saved.
func LoadParallel(dir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "parallel"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 1 || n > 64 {
		return 0, fmt.Errorf("saved parallel: want an integer from 1 to 64")
	}
	return n, nil
}

// SaveParallel atomically saves init's private, durable concurrency choice.
func SaveParallel(dir string, n int) error {
	if n < 1 || n > 64 {
		return fmt.Errorf("parallel: want an integer from 1 to 64")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".parallel-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := fmt.Fprintln(f, n); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "parallel"))
}
