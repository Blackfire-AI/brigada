package lifecycle

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// Reference schemes of an env.<harness>.<VAR> key (§5.1).
const (
	envRefKeychain = "keychain:"
	envRefFile     = "file:"
)

// envKnownHarnesses are the harnesses an env.<harness>.<VAR> key may name.
var envKnownHarnesses = map[string]bool{
	"claude": true, "codex": true, "kimi": true, "opencode": true, "dsh": true,
}

var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// setEnvRef records one env.<harness>.<VAR> key. The config holds the
// reference alone, never a value: a value written where a reference
// belongs is refused without being echoed.
func setEnvRef(cfg *FileConfig, key, value string) error {
	rest, _ := strings.CutPrefix(key, "env.")
	harness, name, ok := strings.Cut(rest, ".")
	if !ok || !envKnownHarnesses[harness] || !envVarName.MatchString(name) {
		return fmt.Errorf("want env.<harness>.<VAR> with a harness of claude, codex, kimi, opencode or dsh")
	}
	if envReserved(name) {
		return fmt.Errorf("%s is set by the runner or the adapter and cannot be configured", name)
	}
	var target string
	switch {
	case strings.HasPrefix(value, envRefKeychain):
		target = strings.TrimPrefix(value, envRefKeychain)
	case strings.HasPrefix(value, envRefFile):
		target = strings.TrimPrefix(value, envRefFile)
	default:
		return fmt.Errorf("want a reference, keychain:<item> or file:<path>; the config never holds a value")
	}
	if target == "" {
		return fmt.Errorf("the reference names nothing")
	}
	if cfg.Env == nil {
		cfg.Env = map[string]map[string]string{}
	}
	if cfg.Env[harness] == nil {
		cfg.Env[harness] = map[string]string{}
	}
	cfg.Env[harness][name] = value
	return nil
}

// envReserved names the variables the runner and the adapters pin: the
// worktree's PWD, brigada's own variables and the permission and telemetry
// pins of the sandboxed harnesses.
func envReserved(name string) bool {
	switch name {
	case "PWD", "DSH_PERMISSION_MODE", "DSH_TELEMETRY_MODE", "OPENCODE_PERMISSION", "OPENCODE_DISABLE_PROJECT_CONFIG":
		return true
	}
	return strings.HasPrefix(name, "BRIGADA_")
}

// SecretCommand runs a program and returns its standard output. It is the
// seam a test replaces; nil means the real program.
type SecretCommand func(name string, args ...string) ([]byte, error)

func runSecretCommand(name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &out
	// Stderr is dropped: nothing the helper prints belongs in an error.
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ResolveHarnessEnv resolves the env.<harness>.<VAR> references of the
// config for the named harnesses into "VAR=value" entries per harness. A
// failure names the key and the reference, never a value. A nil run uses
// the real `security` program.
func ResolveHarnessEnv(refs map[string]map[string]string, harnesses []string, run SecretCommand) (map[string][]string, error) {
	if run == nil {
		run = runSecretCommand
	}
	out := map[string][]string{}
	for i, h := range harnesses {
		if slices.Contains(harnesses[:i], h) {
			continue
		}
		names := make([]string, 0, len(refs[h]))
		for name := range refs[h] {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			ref := refs[h][name]
			value, err := resolveEnvRef(ref, run)
			if err != nil {
				return nil, fmt.Errorf("env.%s.%s = %s: %w", h, name, ref, err)
			}
			out[h] = append(out[h], name+"="+value)
		}
	}
	return out, nil
}

func resolveEnvRef(ref string, run SecretCommand) (string, error) {
	var raw string
	switch {
	case strings.HasPrefix(ref, envRefFile):
		path := strings.TrimPrefix(ref, envRefFile)
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("cannot read the file: %s", pathErrReason(err))
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("the file is not a regular file")
		}
		if info.Mode().Perm() != 0o600 {
			return "", fmt.Errorf("the file has mode %04o; it must be mode 0600 (chmod 600)", info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read the file: %s", pathErrReason(err))
		}
		raw = string(data)
	case strings.HasPrefix(ref, envRefKeychain):
		item := strings.TrimPrefix(ref, envRefKeychain)
		data, err := run("security", "find-generic-password", "-w", "-s", item)
		if err != nil {
			return "", fmt.Errorf("the keychain (macOS `security`) has no readable item %q on %s", item, runtime.GOOS)
		}
		raw = string(data)
	default:
		return "", fmt.Errorf("unknown reference scheme")
	}
	value := strings.TrimRight(raw, "\r\n")
	if value == "" {
		return "", fmt.Errorf("the reference resolves to an empty value")
	}
	return value, nil
}

// pathErrReason is an os error's cause without the path.
func pathErrReason(err error) string {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err.Error()
	}
	return "unreadable"
}
