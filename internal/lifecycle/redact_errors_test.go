package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseErrorsNeverEchoTheLine: a malformed token-file or config line
// may hold a secret, so the error names the line number and what was
// wrong with its shape, never the line's text.
func TestParseErrorsNeverEchoTheLine(t *testing.T) {
	secret := "s3-" + strings.Repeat("q", 10)

	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte("# tokens\nrunner-1 runner "+secret+" extra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := TokenForRole(tokens, "runner")
	if err == nil {
		t.Fatal("a four-field token line was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("token file error %q echoes the line", err)
	}
	if !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "4 fields") {
		t.Errorf("token file error %q, want the line number and field count", err)
	}

	cfg := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfg, []byte("notify = auto\nhook "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadRequiredConfigFile(cfg)
	if err == nil {
		t.Fatal("a config line without `=` was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("config error %q echoes the line", err)
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("config error %q, want the line number", err)
	}
}
