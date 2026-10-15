package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskAutoRetryConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	cfg, err := LoadConfigFile(path)
	if err != nil || cfg.TaskAutoRetry != nil {
		t.Fatalf("absent config = %+v, %v, want engine default", cfg, err)
	}
	for _, value := range []string{"", "true", "false", "invalid"} {
		t.Run(value, func(t *testing.T) {
			body := ""
			if value != "" {
				body = "task_auto_retry = " + value
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfigFile(path)
			if value == "invalid" {
				if err == nil || !strings.Contains(err.Error(), "task_auto_retry") {
					t.Fatalf("invalid boolean error = %v, want named config refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if value == "" {
				if cfg.TaskAutoRetry != nil {
					t.Fatal("omitted key must keep the engine default")
				}
			} else if cfg.TaskAutoRetry == nil || *cfg.TaskAutoRetry != (value == "true") {
				t.Errorf("task_auto_retry = %v, want %s", cfg.TaskAutoRetry, value)
			}
		})
	}
}
