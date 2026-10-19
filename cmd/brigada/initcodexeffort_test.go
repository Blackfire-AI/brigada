package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCodexUserEffortDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	const catalog = `{"models":[
	 {"slug":"gpt-5","visibility":"list","priority":1,
	  "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}],
	  "default_reasoning_level":"medium"},
	 {"slug":"gpt-6.1-sol","visibility":"list","priority":2,
	  "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"}],"default_reasoning_level":"medium"}
	]}`
	for _, tc := range []struct{ name, config, wantGPT5, wantSol string }{
		{"in levels", "model_reasoning_effort = \"high\"\n", "high", "medium"},
		{"not offered by the model", "model_reasoning_effort = \"ultra\"\n", "medium", "medium"},
		{"unset", "", "medium", "medium"},
		{"table key is not top level", "[profiles.x]\nmodel_reasoning_effort = \"high\"\n", "medium", "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeInitPATH(t, "codex")
			writeInitCodexScript(t, "  debug  Diagnostic tools", "  models  Render models", "", "printf '%s\\n' '"+catalog+"'", "exit 89")
			path := filepath.Join(os.Getenv("HOME"), ".codex", "config.toml")
			writeInitModelFixture(t, path, "model = \"gpt-5\"\napi_key = \"never-print-me\"\n"+tc.config)
			got := initCodexModels(context.Background())
			if len(got.entries) != 2 {
				t.Fatalf("entries = %+v", got.entries)
			}
			if got.entries[0].defaultEffort != tc.wantGPT5 || got.entries[1].defaultEffort != tc.wantSol {
				t.Errorf("defaults = %q, %q; want %q, %q", got.entries[0].defaultEffort, got.entries[1].defaultEffort, tc.wantGPT5, tc.wantSol)
			}
			assertInitModelPrivacy(t, got)

			// The menu names the user's own default first, and Enter records it.
			var out bytes.Buffer
			ask := func(string, string) (string, error) { return "", nil }
			effort, err := initPickEffort(&out, "codex", got.entries[0], ask)
			if err != nil || effort != tc.wantGPT5 {
				t.Fatalf("pick = %q, %v; want %q", effort, err, tc.wantGPT5)
			}
			wantNote := "codex's default"
			if tc.wantGPT5 == "high" {
				wantNote = "your codex default"
			}
			compact := strings.Join(strings.Fields(out.String()), "")
			if want := strings.Join(strings.Fields(tc.wantGPT5+" "+wantNote), ""); !strings.Contains(compact, want) {
				t.Errorf("menu lacks %q:\n%s", tc.wantGPT5+" "+wantNote, out.String())
			}
			// brigada config reads the same default.
			model, shown := configAgentDetails(context.Background(), agentSpec{harness: "codex", model: "default"})
			if want := tc.wantGPT5 + " (codex default)"; model != "gpt-5" || shown != want {
				t.Errorf("config shows %q %q; want gpt-5 %q", model, shown, want)
			}
		})
	}
}

func TestInitCurrentTagMovesToRowEnd(t *testing.T) {
	source := initModelSource{entries: []initModelEntry{
		{id: 1, name: "a", value: "a", note: "Ends with a period."},
		{id: 2, name: "b", value: "b"},
	}}
	got := initCurrentModel(source, agentSpec{harness: "claude", model: "a"})
	if got.entries[0].note != "Ends with a period." || !got.entries[0].isCurrent || got.defaultID != 1 {
		t.Errorf("model note = %q, default %d", got.entries[0].note, got.defaultID)
	}
	if source.entries[0].note != "Ends with a period." || source.entries[0].isCurrent {
		t.Errorf("initCurrentModel changed its input: %q", source.entries[0].note)
	}
	if got = initCurrentModel(source, agentSpec{harness: "claude", model: "b"}); got.entries[1].note != "" || !got.entries[1].isCurrent {
		t.Errorf("bare note = %q", got.entries[1].note)
	}

	var out bytes.Buffer
	entry := initModelEntry{name: "m", effortLevels: []string{"low", "high"}, defaultEffort: "low"}
	ask := func(string, string) (string, error) { return "", nil }
	if _, err := initPickEffort(&out, "codex", entry, ask, "high"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"low   codex's default\n", "high  current\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("effort menu lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if _, err := initPickEffort(&out, "codex", entry, ask, "low"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "low   current\n") {
		t.Errorf("effort menu current tag:\n%s", out.String())
	}
}
