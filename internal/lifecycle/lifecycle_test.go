package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestAcquireLockRefusesASecondHolderNamingThePid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.lock")
	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()

	second, err := AcquireLock(path)
	if err == nil {
		second.Release()
		t.Fatal("a second engine took the same lock; want refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pid ") || !strings.Contains(msg, "already running") {
		t.Errorf("refusal = %q, want it to name the live engine's pid", msg)
	}
	if !strings.Contains(msg, "refusing to start a second") {
		t.Errorf("refusal = %q, want it to say what was refused", msg)
	}

	// Release frees it for the next engine.
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	again.Release()
}

func TestEnginePidReadsTheHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.lock")
	if pid := EnginePid(path); pid != 0 {
		t.Fatalf("EnginePid with no lock file = %d, want 0", pid)
	}
	lock, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()
	if pid := EnginePid(path); pid <= 0 {
		t.Fatalf("EnginePid under a live engine = %d, want the holder's pid", pid)
	}
}

// TestIdleMonitorIsDrivenByTheClock covers §13's idle rule as a unit: the
// monitor reads busyness on the injected clock, never exits while the
// engine has something to do, and exits once the quiet has lasted.
func TestIdleMonitorIsDrivenByTheClock(t *testing.T) {
	start := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	busy := false
	m := newIdleMonitor(30*time.Minute, func() bool { return busy }, start)

	steps := []struct {
		at   time.Duration // from start
		work bool
		exit bool
	}{
		{0, false, false}, // born idle, but the clock has not moved
		{10 * time.Minute, false, false},
		{29 * time.Minute, true, false},  // work resets the clock
		{39 * time.Minute, false, false}, // 10m since work: not yet
		{60 * time.Minute, false, true},  // 30m since work: idle out
	}
	for _, s := range steps {
		now := start.Add(s.at)
		busy = s.work
		m.poll(now)
		if got := m.shouldExit(now); got != s.exit {
			t.Errorf("at %v: shouldExit = %v, want %v (last busy %v)", s.at, got, s.exit, m.lastBusy)
		}
	}

	// A live job is busyness for as long as it lasts.
	busy = true
	m.poll(start.Add(61 * time.Minute))
	if m.shouldExit(start.Add(61 * time.Minute)) {
		t.Error("a busy engine must not idle out")
	}
}

func TestConfigResolveChecksTheSocketAndFillsDefaults(t *testing.T) {
	dir := testsupport.ShortStateDir(t)
	cfg := Config{StateDir: dir}
	p, err := cfg.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.Socket != filepath.Join(dir, "brigada.sock") || p.Store != filepath.Join(dir, "store.db") ||
		p.Lock != filepath.Join(dir, "engine.lock") || p.Tokens != filepath.Join(dir, "tokens") {
		t.Errorf("paths = %+v, want everything inside the state dir", p)
	}
	if cfg.Idle != defaultIdle {
		t.Errorf("idle = %v, want the default %v", cfg.Idle, defaultIdle)
	}
	if cfg.IdleTick <= 0 || cfg.IdleTick > cfg.Idle {
		t.Errorf("idle tick = %v, want inside (0, idle]", cfg.IdleTick)
	}
	if cfg.Now == nil {
		t.Error("resolve left the clock nil")
	}

	// A socket path past the kernel's cap is refused up front, naming it.
	deep := filepath.Join(dir, strings.Repeat("d/", 60), "brigada.sock")
	err = CheckSocketPath(deep)
	if err == nil || !strings.Contains(err.Error(), "104") || !strings.Contains(err.Error(), "shorter state directory") {
		t.Errorf("over-cap socket refusal = %v, want the cap and the fix named", err)
	}
	cfg = Config{StateDir: dir, Socket: deep}
	if _, err := cfg.resolve(); err == nil {
		t.Error("resolve accepted an over-cap socket path; want refusal")
	}

	var empty Config
	if _, err := empty.resolve(); err == nil {
		t.Error("resolve without a state dir succeeded; want refusal")
	}
}

func TestConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if _, err := LoadConfigFile(filepath.Join(dir, "absent")); err != nil {
		t.Errorf("a missing config file must read as empty: %v", err)
	}
	if err := os.WriteFile(path, []byte("# a comment\n\nstate_dir = /elsewhere\nidle_minutes = 1.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StateDir != "/elsewhere" || cfg.IdleMinutes != 1.5 {
		t.Errorf("cfg = %+v, want the file's values", cfg)
	}
	for name, body := range map[string]string{
		"unknown key": "frobnicate = 1\n",
		"no equals":   "state_dir\n",
		"bad number":  "idle_minutes = soon\n",
		"negative":    "idle_minutes = -1\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfigFile(path); err == nil {
			t.Errorf("%s: accepted, want refusal", name)
		}
	}
}

func TestEnsureTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	if err := EnsureTokenFile(path); err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %04o, want 0600", info.Mode().Perm())
	}
	for role, wantName := range map[string]string{"client": TokenNameClient, "admin": TokenNameAdmin, "runner": TokenNameRunner} {
		name, secret, err := TokenForRole(path, role)
		if err != nil {
			t.Fatalf("read the %s token: %v", role, err)
		}
		if name != wantName || len(secret) < 32 {
			t.Errorf("%s token = %s/%q, want %s and a long hex secret", role, name, secret, wantName)
		}
	}
	// A second run keeps the file: secrets must survive engine restarts.
	_, before, _ := TokenForRole(path, "client")
	if err := EnsureTokenFile(path); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	_, after, _ := TokenForRole(path, "client")
	if before != after {
		t.Error("EnsureTokenFile rewrote an existing token file")
	}
	if _, _, err := TokenForRole(path, "foreman"); err == nil || !strings.Contains(err.Error(), "no foreman token") {
		t.Errorf("a missing role = %v, want an error naming it", err)
	}
}
