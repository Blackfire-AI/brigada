package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxSocketPath is the longest Unix socket path the kernel accepts:
// sockaddr_un.sun_path holds 104 bytes on the tightest system (108 on
// Linux), and a bind past the cap fails with a bare "invalid argument"
// that names nothing. Paths at or over the cap are refused up front,
// before anything is started, with a message that says what to do.
const maxSocketPath = 104

// DefaultStateDir is where the engine's state lives when nothing names
// another place (§2): $XDG_STATE_HOME/brigada, else
// ~/.local/state/brigada.
func DefaultStateDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "brigada")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "brigada" // no home to build on; relative, and the caller decides
	}
	return filepath.Join(home, ".local", "state", "brigada")
}

// Paths are the engine's files inside one state directory: the store,
// the API's socket, the single-instance lock beside the store (§13), the
// token file (§6.4) and the log a detached engine writes.
type Paths struct {
	StateDir string
	Store    string
	Socket   string
	Lock     string
	Tokens   string
	Log      string
}

// PathsOf derives every path from stateDir.
func PathsOf(stateDir string) Paths {
	return Paths{
		StateDir: stateDir,
		Store:    filepath.Join(stateDir, "store.db"),
		Socket:   filepath.Join(stateDir, "brigada.sock"),
		Lock:     filepath.Join(stateDir, "engine.lock"),
		Tokens:   filepath.Join(stateDir, "tokens"),
		Log:      filepath.Join(stateDir, "engine.log"),
	}
}

// CheckSocketPath refuses a socket path the kernel could not bind,
// naming the length and the cap so the fix — a shorter state directory —
// is on the line.
func CheckSocketPath(path string) error {
	if len(path) >= maxSocketPath {
		return fmt.Errorf("socket path %s is %d bytes; a Unix socket path is capped at %d bytes (sun_path); use a shorter state directory",
			path, len(path), maxSocketPath)
	}
	return nil
}

// FileConfig is the user's config file (§2): the settings that do not
// change per invocation. One `key = value` per line; `#` comments and
// blank lines allowed; unknown keys are refused, so a typo cannot
// silently tune nothing.
type FileConfig struct {
	StateDir    string
	IdleMinutes float64 // 0 = the default
}

// DefaultConfigPath is where the config file lives when nothing names
// another place: $XDG_CONFIG_HOME/brigada/config, else
// ~/.config/brigada/config.
func DefaultConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "brigada", "config")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join("brigada", "config")
	}
	return filepath.Join(home, ".config", "brigada", "config")
}

// LoadConfigFile reads the config file. A missing file is the empty
// config, not an error: most installs have none.
func LoadConfigFile(path string) (FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileConfig{}, nil
		}
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return FileConfig{}, fmt.Errorf("config %s: line %d: want `key = value`, got %q", path, n+1, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		var err error
		switch key {
		case "state_dir":
			cfg.StateDir = value
		case "idle_minutes":
			cfg.IdleMinutes, err = strconv.ParseFloat(value, 64)
			if err == nil && cfg.IdleMinutes < 0 {
				err = fmt.Errorf("negative")
			}
			if err != nil {
				return FileConfig{}, fmt.Errorf("config %s: line %d: idle_minutes: %v", path, n+1, err)
			}
		default:
			return FileConfig{}, fmt.Errorf("config %s: line %d: unknown key %q; want state_dir or idle_minutes", path, n+1, key)
		}
	}
	return cfg, nil
}
