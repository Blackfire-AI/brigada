package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retention_days (§9) reads a whole number of days; absent is 0, keep
// everything, and a negative or non-integer value is refused by name.
func TestRetentionDaysConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	for _, tc := range []struct {
		body string
		want int
		bad  bool
	}{
		{"", 0, false},
		{"retention_days = 0", 0, false},
		{"retention_days = 30", 30, false},
		{"retention_days = -1", 0, true},
		{"retention_days = a month", 0, true},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfigFile(path)
		if tc.bad {
			if err == nil || !strings.Contains(err.Error(), "retention_days") {
				t.Errorf("%q: err = %v, want a refusal naming retention_days", tc.body, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tc.body, err)
		}
		if cfg.RetentionDays != tc.want {
			t.Errorf("%q: RetentionDays = %d, want %d", tc.body, cfg.RetentionDays, tc.want)
		}
	}
}
