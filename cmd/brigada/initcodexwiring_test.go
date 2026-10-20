package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wireCodexTable appends the [mcp_servers.brigada] table an older init
// wrote, behind the blank separator init added.
func wireCodexTable(t *testing.T, root string) {
	t.Helper()
	base := ""
	if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(wiringCodexFile))); err == nil {
		base = string(data)
	}
	if base != "" && !strings.HasSuffix(base, "\n") {
		base += "\n"
	}
	if strings.TrimSpace(base) != "" && !strings.HasSuffix(base, "\n\n") {
		base += "\n"
	}
	base += "[mcp_servers.brigada]\ncommand = \"brigada\"\nargs = [\"mcp\", \"--foreman\"]\n"
	wireWrite(t, root, wiringCodexFile, base)
}

func TestCodexRemovalRefusesDamagedFiles(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"duplicate tables", "[mcp_servers.brigada]\ncommand = \"brigada\"\n\n[mcp_servers.brigada]\ncommand = \"brigada\"\n"},
		{"inline root", "mcp_servers = { brigada = {} }\n"},
		{"dotted inline", "mcp_servers.brigada = { command = \"brigada\" }\n"},
		{"dotted member", "mcp_servers.brigada.command = \"x\"\n"},
		{"inline under mcp_servers", "[mcp_servers]\nbrigada = { command = \"x\" }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, wiringCodexFile, tc.body)
			removal := planRemoval(root, false)
			for _, r := range removal {
				if r.rel == wiringCodexFile && (r.err == nil || r.changed()) {
					t.Fatalf("remove not refused: state %s err %v", r.state, r.err)
				}
			}
			if err := applyRemoval(removal); err != nil {
				t.Fatal(err)
			}
			if got := wireRead(t, root, wiringCodexFile); got != tc.body {
				t.Fatalf("damaged file rewritten by remove: %q", got)
			}
		})
	}
}

func TestCodexRemovalReversesWiring(t *testing.T) {
	t.Run("fresh file deleted", func(t *testing.T) {
		root := wireRepo(t)
		wireCodexTable(t, root)
		if err := applyRemoval(planRemoval(root, false)); err != nil {
			t.Fatal(err)
		}
		if fileExists(filepath.Join(root, wiringCodexFile)) || fileExists(filepath.Join(root, ".codex")) {
			t.Fatal("file or .codex directory remains")
		}
	})
	t.Run("user content kept", func(t *testing.T) {
		for _, old := range []string{
			"model = \"gpt-6.1-sol\"\n",
			"[other]\nkey = 1\n\n[mcp_servers.mine]\ncommand = \"mine\"\n",
		} {
			root := wireRepo(t)
			wireWrite(t, root, wiringCodexFile, old)
			wireCodexTable(t, root)
			if err := applyRemoval(planRemoval(root, false)); err != nil {
				t.Fatal(err)
			}
			if got := wireRead(t, root, wiringCodexFile); got != old {
				t.Fatalf("round trip:\n%q\nwant\n%q", got, old)
			}
		}
	})
	t.Run("table in the middle", func(t *testing.T) {
		root := wireRepo(t)
		old := "[a]\nx = 1\n\n[mcp_servers.brigada]\ncommand = \"brigada\"\nargs = [\"mcp\", \"--foreman\"]\n\n[b]\ny = 2\n"
		wireWrite(t, root, wiringCodexFile, old)
		if err := applyRemoval(planRemoval(root, false)); err != nil {
			t.Fatal(err)
		}
		want := "[a]\nx = 1\n\n[b]\ny = 2\n"
		if got := wireRead(t, root, wiringCodexFile); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("no table", func(t *testing.T) {
		root := wireRepo(t)
		old := "model = \"gpt-6.1-sol\"\n"
		wireWrite(t, root, wiringCodexFile, old)
		files := planRemoval(root, false)
		for _, f := range files {
			if f.rel == wiringCodexFile && (f.changed() || f.err != nil) {
				t.Errorf("planned a change: state %s err %v", f.state, f.err)
			}
		}
		if err := applyRemoval(files); err != nil {
			t.Fatal(err)
		}
		if got := wireRead(t, root, wiringCodexFile); got != old {
			t.Fatalf("rewrote %q", got)
		}
	})
}
