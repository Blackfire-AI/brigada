package runnerstate

import (
	"os"
	"path/filepath"
	"strings"
)

// LoadAgents reads the durable choices, independently of runner liveness.
// A nil slice means init has not saved choices yet.
func LoadAgents(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "agents"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []string{}, nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), nil
}

// AppendAgent adds one line to the agents file and keeps every line already
// in it, comments and lines it does not understand included.
func AppendAgent(dir, line string) error {
	lines, err := LoadAgents(dir)
	if err != nil {
		return err
	}
	return SaveAgents(dir, append(lines, line))
}

// SaveAgents atomically replaces the private, durable runner choices. The
// live runner.json record is published separately by the runner itself.
func SaveAgents(dir string, specs []string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".agents-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	data := strings.Join(specs, "\n")
	if len(specs) > 0 {
		data += "\n"
	}
	if _, err := f.WriteString(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "agents"))
}
