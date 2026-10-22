package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTokenFileErrorsNeverEchoTheLine: a malformed line may be a secret
// in the wrong column, so the error names the line number and the shape
// that was wrong, never the line's words.
func TestTokenFileErrorsNeverEchoTheLine(t *testing.T) {
	secret := "s3-" + strings.Repeat("q", 10)
	for name, line := range map[string]string{
		"fields":        "runner-1 runner " + secret + " extra",
		"unknown role":  "runner-1 " + secret + " other-secret",
		"foreman scope": "runner-1 foreman:" + secret + " other-secret",
	} {
		path := filepath.Join(t.TempDir(), "tokens")
		if err := os.WriteFile(path, []byte("# tokens\n"+line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadTokenFile(path)
		if err == nil {
			t.Errorf("%s: a malformed line was accepted", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error %q echoes the line", name, err)
		}
		if !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%s: error %q, want the line number", name, err)
		}
	}
}
