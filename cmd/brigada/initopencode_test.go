package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestInitOpenCodeSignInCheck(t *testing.T) {
	const providersHelp = `  opencode providers   manage AI providers and credentials   [aliases: auth]`
	const authHelp = `  opencode auth Manage credentials`
	const two = `┌ Credentials\n● Provider one oauth\n● Provider two api\n└ 2 credentials\n`
	const zero = `┌ Credentials\n└ 0 credentials\n`
	const unchecked = "sign-in can't be checked"
	const noStored = "no stored provider credentials (keys may also come from your environment)"
	for _, tc := range []struct {
		name, help, list, command string
		listExit                  int
		wantSignedIn, wantChecked bool
		wantCount                 int
		wantStatus                string
	}{
		{"providers two", providersHelp, two, "providers", 0, true, true, 2, "signed in to 2 providers (opencode providers list)"},
		{"providers zero", providersHelp, zero, "providers", 0, false, true, 0, noStored},
		{"old auth row two", authHelp, two, "auth", 0, true, true, 2, "signed in to 2 providers (opencode auth list)"},
		{"old auth row zero", authHelp, zero, "auth", 0, false, true, 0, noStored},
		{"providers list fails", providersHelp, two, "providers", 3, false, false, 0, unchecked},
		{"neither documented", `  opencode models [provider]`, two, "", 0, false, false, 0, unchecked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeInitPATH(t, "opencode")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s %%s\\n' \"${0##*/}\" \"$*\" >> \"$INIT_CALLS\"\ncase \"$*\" in\n --help) printf '  opencode models [provider]\\n%s\\n' ;;\n models) printf 'fixture/gpt-5\\n' ;;\n '%s list') printf '%s'; exit %d ;;\n *) exit 89 ;;\nesac\n", tc.help, tc.command, tc.list, tc.listExit)
			if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "opencode"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			source := initOpenCodeModels(context.Background())
			p := source.signIn
			if p.signedIn != tc.wantSignedIn || p.checked != tc.wantChecked || source.providerCount != tc.wantCount || !strings.Contains(p.status, tc.wantStatus) {
				t.Errorf("signIn = %+v count %d, want signedIn %v checked %v count %d status %q", p, source.providerCount, tc.wantSignedIn, tc.wantChecked, tc.wantCount, tc.wantStatus)
			}
			if tc.listExit != 0 && len(source.entries) != 0 {
				t.Errorf("models kept after a failed list: %+v", source.entries)
			}
			data, _ := os.ReadFile(calls)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.HasSuffix(line, " --help") && line != "opencode models" && line != "opencode "+tc.command+" list" {
					t.Errorf("unexpected invocation %q", line)
				}
			}
		})
	}
}

func TestInitOpenCodeNoStoredCredentialsAsksUse(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, "claude", "codex", "opencode")
	path := filepath.Join(os.Getenv("PATH"), "opencode")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.ReplaceAll(script, []byte(`● Provider one oauth\n● Provider two api\n└ 2 credentials\n`), []byte(`└ 0 credentials\n`))
	if err := os.WriteFile(path, script, 0755); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n\n\n\n\n\nn\n", "--signed-in", "claude,codex", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 1 || errOut != "" {
		t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "  opencode: no stored provider credentials (keys may also come from your\n  environment)\n    Use opencode? [y/N]:") {
		t.Errorf("missing no-credentials status before the Use question:\n%s", out)
	}
	if strings.Contains(out, "init can't check sign-in. If you're not signed in, run\n  `opencode") {
		t.Errorf("printed can't-check guidance after a successful check:\n%s", out)
	}
}
