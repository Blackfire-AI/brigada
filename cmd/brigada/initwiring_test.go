package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func wireWrite(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wireRead(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func wireApply(t *testing.T, root string) []wireFile {
	t.Helper()
	files := planWiring(root)
	if err := applyWiring(files); err != nil {
		t.Fatal(err)
	}
	return files
}

func wireJSON(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}
	return v
}

// keepsBytes reports whether every byte of old appears in order in next: an
// edit that only inserts leaves the user's bytes untouched.
func keepsBytes(old, next string) bool {
	i := 0
	for j := 0; j < len(next) && i < len(old); j++ {
		if next[j] == old[i] {
			i++
		}
	}
	return i == len(old)
}

func TestWiringFreshFilesHoldExactlyBrigadasEntries(t *testing.T) {
	root := t.TempDir()
	wireApply(t, root)
	wantMCP := map[string]any{"mcpServers": map[string]any{"brigada": map[string]any{"command": "brigada", "args": []any{"mcp", "--foreman"}}}}
	if got := wireJSON(t, wireRead(t, root, ".mcp.json")); !reflect.DeepEqual(got, wantMCP) {
		t.Errorf(".mcp.json = %v", got)
	}
	hook := func(cmd string) []any {
		return []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}}}
	}
	wantSettings := map[string]any{"hooks": map[string]any{"SessionStart": hook("brigada hold confirm"), "Stop": hook("brigada hold yield")}}
	if got := wireJSON(t, wireRead(t, root, ".claude/settings.json")); !reflect.DeepEqual(got, wantSettings) {
		t.Errorf("settings.json = %v", got)
	}
	skill := wireRead(t, root, ".claude/skills/brigada-foreman/SKILL.md")
	if !strings.HasPrefix(skill, "---\nname: brigada-foreman\n") || !strings.Contains(skill, wiringSkillMarker) || !strings.Contains(skill, "hold_acquire") {
		t.Errorf("skill = %.200s", skill)
	}
	for _, rel := range []string{".mcp.json", ".claude/settings.json"} {
		for _, secret := range []string{"token", "secret", root} {
			if strings.Contains(wireRead(t, root, rel), secret) {
				t.Errorf("%s mentions %q", rel, secret)
			}
		}
	}
}

func TestWiringKeepsOthersAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	mcp := "{\n\t\"mcpServers\": {\n\t\t\"other\": { \"command\": \"x\", \"args\": [] }\n\t},\n\t\"extra\": [1,  2]\n}\n"
	settings := "{\n    \"model\": \"opus\",\n    \"hooks\": {\n        \"Stop\": [\n            {\"hooks\": [{\"type\": \"command\", \"command\": \"mine\"}]}\n        ],\n        \"PreToolUse\": []\n    }\n}\n"
	claudeMD := "# mine\n"
	wireWrite(t, root, ".mcp.json", mcp)
	wireWrite(t, root, ".claude/settings.json", settings)
	wireWrite(t, root, "CLAUDE.md", claudeMD)
	wireApply(t, root)

	gotMCP, gotSettings := wireRead(t, root, ".mcp.json"), wireRead(t, root, ".claude/settings.json")
	if !keepsBytes(mcp, gotMCP) || !keepsBytes(settings, gotSettings) {
		t.Errorf("user bytes changed:\n%s\n%s", gotMCP, gotSettings)
	}
	if !strings.Contains(gotMCP, "\t\t\"brigada\": {") {
		t.Errorf("entry does not follow the file's tab layout:\n%s", gotMCP)
	}
	if wireRead(t, root, "CLAUDE.md") != claudeMD {
		t.Error("CLAUDE.md touched")
	}
	m := wireJSON(t, gotMCP).(map[string]any)
	servers := m["mcpServers"].(map[string]any)
	if len(servers) != 2 || servers["other"] == nil || servers["brigada"] == nil || m["extra"] == nil {
		t.Errorf("servers = %v", m)
	}
	s := wireJSON(t, gotSettings).(map[string]any)
	hooks := s["hooks"].(map[string]any)
	if s["model"] != "opus" || len(hooks["Stop"].([]any)) != 2 || len(hooks["SessionStart"].([]any)) != 1 || hooks["PreToolUse"] == nil {
		t.Errorf("settings = %v", s)
	}
	if !strings.HasPrefix(strings.SplitN(gotSettings, "\"Stop\"", 2)[1], ": [\n            {\"hooks\": [{\"type\": \"command\", \"command\": \"mine\"}]}") {
		t.Errorf("the user's hook moved:\n%s", gotSettings)
	}

	again := planWiring(root)
	for _, f := range again {
		if f.err != nil || f.changed() || f.state != "unchanged" {
			t.Errorf("re-run on %s: state %s err %v", f.rel, f.state, f.err)
		}
	}
	if err := applyWiring(again); err != nil {
		t.Fatal(err)
	}
	if wireRead(t, root, ".mcp.json") != gotMCP || wireRead(t, root, ".claude/settings.json") != gotSettings {
		t.Error("re-run rewrote a file")
	}
}

func TestWiringReplacesBrigadasEntriesInPlace(t *testing.T) {
	root := t.TempDir()
	mcp := "{\"mcpServers\":{\"a\":{\"command\":\"a\"},\"brigada\":{\"command\":\"/old/brigada\"},\"z\":{\"command\":\"z\"}}}"
	settings := "{\"hooks\":{\"Stop\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"before\"}]},{\"hooks\":[{\"type\":\"bogus\",\"command\":\"brigada hold yield\"}]},{\"hooks\":[{\"type\":\"command\",\"command\":\"after\"}]}],\"SessionStart\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"brigada hold confirm\"}]}]}}"
	wireWrite(t, root, ".mcp.json", mcp)
	wireWrite(t, root, ".claude/settings.json", settings)
	wireApply(t, root)
	wantMCP := strings.Replace(mcp, "{\"command\":\"/old/brigada\"}", "{\"command\":\"brigada\",\"args\":[\"mcp\",\"--foreman\"]}", 1)
	if got := wireRead(t, root, ".mcp.json"); got != wantMCP {
		t.Errorf("mcp:\n%s\nwant\n%s", got, wantMCP)
	}
	wantSettings := strings.Replace(settings, "\"type\":\"bogus\"", "\"type\":\"command\"", 1)
	if got := wireRead(t, root, ".claude/settings.json"); got != wantSettings {
		t.Errorf("settings:\n%s\nwant\n%s", got, wantSettings)
	}
}

func TestWiringDamagedFilesAreReportedAndLeftAlone(t *testing.T) {
	for _, tc := range []struct{ name, rel, body string }{
		{"truncated mcp", ".mcp.json", "{\"mcpServers\": "},
		{"mcp servers not an object", ".mcp.json", "{\"mcpServers\": []}"},
		{"mcp top level array", ".mcp.json", "[]"},
		{"empty settings", ".claude/settings.json", ""},
		{"hooks not an object", ".claude/settings.json", "{\"hooks\": []}"},
		{"stop not an array", ".claude/settings.json", "{\"hooks\": {\"Stop\": {}}}"},
		{"settings with comments", ".claude/settings.json", "{\n// no\n}"},
		{"foreign skill", ".claude/skills/brigada-foreman/SKILL.md", "my own skill\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			wireWrite(t, root, tc.rel, tc.body)
			files := planWiring(root)
			var hit bool
			for _, f := range files {
				if f.rel == tc.rel {
					hit = true
					if f.err == nil || f.changed() {
						t.Errorf("%s not reported: state %s err %v", f.rel, f.state, f.err)
					}
				}
			}
			if !hit {
				t.Fatal("file not planned")
			}
			if err := applyWiring(files); err != nil {
				t.Fatal(err)
			}
			if got := wireRead(t, root, tc.rel); got != tc.body {
				t.Errorf("damaged file rewritten: %q", got)
			}
		})
	}
}

func TestWiringSkillRefreshesOnlyItsOwnFile(t *testing.T) {
	root := t.TempDir()
	rel := ".claude/skills/brigada-foreman/SKILL.md"
	wireWrite(t, root, rel, "---\nname: brigada-foreman\n---\n"+wiringSkillMarker+" v0 -->\nold text\n")
	wireApply(t, root)
	if got := wireRead(t, root, rel); strings.Contains(got, "old text") || !strings.Contains(got, "hold_acquire") {
		t.Errorf("skill not refreshed: %.200s", got)
	}
}

func TestInitProbeListsWiringWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		extra      []string
		wantWiring bool
	}{
		{"wiring", nil, true},
		{"no wiring", []string{"--no-wiring"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			if err := os.WriteFile("go.mod", []byte("module example.invalid/test\n"), 0644); err != nil {
				t.Fatal(err)
			}
			calls := fakeInitPATH(t, "claude", "codex")
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			args := append([]string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}, tc.extra...)
			code, out, errOut := initDo(t, state, "", args...)
			if code != 0 {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			for _, want := range []string{"Harness wiring", ".mcp.json: created", ".claude/settings.json: created", "brigada-foreman/SKILL.md: created", "codex mcp add brigada-foreman -- brigada mcp --foreman", "auto_review"} {
				if strings.Contains(out, want) != tc.wantWiring {
					t.Errorf("%q present = %v, want %v\n%s", want, !tc.wantWiring, tc.wantWiring, out)
				}
			}
			for _, rel := range []string{".mcp.json", ".claude"} {
				if _, err := os.Stat(rel); !os.IsNotExist(err) {
					t.Errorf("probe wrote %s: %v", rel, err)
				}
			}
			data, _ := os.ReadFile(calls)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.HasSuffix(line, " --help") {
					t.Errorf("harness invoked beyond --help (codex command must be printed, not run): %q", line)
				}
			}
		})
	}
}

func TestHoldConfirmAlwaysExitsZero(t *testing.T) {
	for _, args := range [][]string{{"hold", "confirm"}, {"hold", "confirm", "--session", "s1"}, {"hold", "confirm", "--bogus"}} {
		var out, errOut strings.Builder
		if code := run(context.Background(), args, &out, &errOut); code != 0 {
			t.Errorf("%v exited %d: %s", args, code, errOut.String())
		}
	}
}

func TestInitProbeShowsDiffOfExistingWiring(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, "claude")
	wireWrite(t, ".", ".mcp.json", "{\n  \"mcpServers\": {\n    \"other\": {\"command\": \"x\"}\n  }\n}\n")
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	code, out, errOut := initDo(t, state, "", "--probe", "--check", "true", "--tools", "none", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude")
	if code != 0 {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "+++ b/.mcp.json") || !strings.Contains(out, "+        \"brigada\": {") && !strings.Contains(out, "+    \"brigada\": {") {
		t.Errorf("no diff for .mcp.json:\n%s", out)
	}
	if got := wireRead(t, ".", ".mcp.json"); !strings.Contains(got, "\"other\"") || strings.Contains(got, "brigada") {
		t.Errorf("probe changed .mcp.json: %s", got)
	}
}
