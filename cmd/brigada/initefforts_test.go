package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func assertInitEfforts(t *testing.T, entry initModelEntry, levels []string, defaultEffort string) {
	t.Helper()
	if !reflect.DeepEqual(entry.effortLevels, levels) || entry.defaultEffort != defaultEffort {
		t.Errorf("model %q: efforts = %v, default = %q; want %v, %q", entry.name, entry.effortLevels, entry.defaultEffort, levels, defaultEffort)
	}
}

func TestInitCodexEfforts(t *testing.T) {
	t.Chdir(t.TempDir())
	const catalog = `{"models":[
	 {"slug":"gpt-5","visibility":"list","priority":1,
	  "supported_reasoning_levels":[{"effort":"low","description":"never-print-me"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra","private":{"effort":"never-print-me"}}],
	  "default_reasoning_level":"medium","other":{"effort":"never-print-me"}},
	 {"slug":"gpt-6.1-sol","visibility":"list","priority":2,
	  "supported_reasoning_levels":[{"effort":"high"},{"effort":"low"}],"default_reasoning_level":"high"},
	 {"slug":"gpt-5.4-mini","visibility":"list","priority":3}
	]}`
	for _, defaultModel := range []string{"gpt-5", "gpt-config-only", ""} {
		t.Run("default="+defaultModel, func(t *testing.T) {
			fakeInitPATH(t, "codex")
			writeInitCodexScript(t, "  debug  Diagnostic tools", "  models  Render models", "", "printf '%s\\n' '"+catalog+"'", "exit 89")
			writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".codex", "config.toml"), fmt.Sprintf("model = %q\n", defaultModel))
			got := initCodexModels(context.Background())
			wantCount := 4
			if defaultModel == "gpt-5" {
				wantCount = 3
			}
			if len(got.entries) != wantCount {
				t.Fatalf("entries = %+v", got.entries)
			}
			for _, entry := range got.entries {
				switch entry.name {
				case "gpt-5":
					assertInitEfforts(t, entry, []string{"low", "medium", "high", "xhigh", "max", "ultra"}, "medium")
				case "gpt-6.1-sol":
					assertInitEfforts(t, entry, []string{"high", "low"}, "high")
				default:
					assertInitEfforts(t, entry, nil, "")
				}
			}
			assertInitModelPrivacy(t, got)
		})
	}
}

func TestInitOpenCodeEfforts(t *testing.T) {
	const listing = `fixture/gpt-5
{"status":"active","variants":{"max":{"reasoningEffort":"never-print-me","nested":[{"token":"never-print-me"}]},"low":{},"medium":{"private":"never-print-me"},"high":{},"xhigh":{}},"default_effort":"never-print-me","private":{"variants":{"never-print-me":{}}}}
fixture/glm-5.3
{"status":"active","variants":{}}
fixture/gpt-6.1-sol
{"status":"active"}
`
	fakeOpenCodeVerbose(t, openCodeVerboseHelp, openCodeVerboseFile(t, listing))
	got := initOpenCodeModels(context.Background())
	if len(got.entries) != 3 {
		t.Fatalf("entries = %+v", got.entries)
	}
	assertInitEfforts(t, got.entries[0], []string{"max", "low", "medium", "high", "xhigh"}, "")
	assertInitEfforts(t, got.entries[1], nil, "")
	assertInitEfforts(t, got.entries[2], nil, "")
	assertInitModelPrivacy(t, got)
}

func TestInitKimiEfforts(t *testing.T) {
	fakeInitPATH(t, "kimi")
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".kimi-code", "config.toml"), `default_model = "kimi-code/current"
support_efforts = ["never-print-me"]
default_effort = "never-print-me"
secret = '''
[models."never-print-me"]
support_efforts = ["never-print-me"]
default_effort = "never-print-me"
'''
[providers."kimi-code"]
support_efforts = ["never-print-me"]
default_effort = "never-print-me"
[models."kimi-code/current"]
support_efforts = ["low", 'medium', "high"] # keep array order
default_effort = "medium" # current default
private = "never-print-me"
[models."kimi-code/other"]
support_efforts = [
  "high", # keep a different order
  # skipped comment
  'low',
]
default_effort = 'high'
[models."kimi-code/current"] # metadata survives repeated tables
[models."kimi-code/bare"]
provider = "never-print-me"
[models."kimi-code/empty"]
support_efforts = []
`)
	got := initKimiModels(context.Background())
	if len(got.entries) != 4 {
		t.Fatalf("entries = %+v", got.entries)
	}
	assertInitEfforts(t, got.entries[0], []string{"low", "medium", "high"}, "medium")
	assertInitEfforts(t, got.entries[1], []string{"high", "low"}, "high")
	assertInitEfforts(t, got.entries[2], nil, "")
	assertInitEfforts(t, got.entries[3], []string{}, "")
	assertInitModelPrivacy(t, got)
}
