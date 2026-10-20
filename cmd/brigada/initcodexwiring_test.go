package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func codexApply(t *testing.T, root string) []wireFile {
	t.Helper()
	files := planCodexWiring(root)
	if err := applyWiring(files); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCodexWiringFreshThenUnchanged(t *testing.T) {
	root := wireRepo(t)
	if f := codexApply(t, root)[0]; f.err != nil || f.state != "created" {
		t.Fatalf("state %s err %v", f.state, f.err)
	}
	want := "[mcp_servers.brigada]\ncommand = \"brigada\"\nargs = [\"mcp\", \"--foreman\"]\n"
	if got := wireRead(t, root, wiringCodexFile); got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
	for _, f := range codexApply(t, root) {
		if f.err != nil || f.changed() || f.state != "unchanged" {
			t.Errorf("rerun: state %s err %v", f.state, f.err)
		}
	}
	if got := wireRead(t, root, wiringCodexFile); got != want {
		t.Errorf("rerun rewrote the file: %q", got)
	}
}

func TestCodexWiringKeepsUserBytes(t *testing.T) {
	for _, tc := range []struct{ name, old string }{
		{"top-level keys", "model = \"gpt-6.1-sol\"\nmodel_reasoning_effort = \"high\"\n"},
		{"other table", "[mcp_servers.other]\ncommand = \"other\"\nargs = []\n"},
		{"table with comments", "# my config\n[other]\nkey = \"value\" # keep\n"},
		{"no trailing newline", "model = \"gpt-6.1-sol\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, wiringCodexFile, tc.old)
			if f := codexApply(t, root)[0]; f.err != nil || f.state != "added" {
				t.Fatalf("state %s err %v", f.state, f.err)
			}
			got := wireRead(t, root, wiringCodexFile)
			if !keepsBytes(tc.old, got) {
				t.Errorf("user bytes changed:\n%s", got)
			}
			want := tc.old
			if !strings.HasSuffix(want, "\n") {
				want += "\n"
			}
			if strings.TrimSpace(want) != "" && !strings.HasSuffix(want, "\n\n") {
				want += "\n"
			}
			want += "[mcp_servers.brigada]\ncommand = \"brigada\"\nargs = [\"mcp\", \"--foreman\"]\n"
			if got != want {
				t.Fatalf("file:\n%q\nwant:\n%q", got, want)
			}
		})
	}
}

func TestCodexWiringReplacesBrigadasTableInPlace(t *testing.T) {
	for _, tc := range []struct{ name, old string }{
		{"stale command", "[mcp_servers.brigada]\ncommand = \"/old/brigada\"\n"},
		{"extra entry", "[mcp_servers.brigada]\ncommand = \"brigada\"\nargs = [\"mcp\", \"--foreman\"]\nenv = { HOME = \"/x\" }\n"},
		{"quoted header", "[mcp_servers.\"brigada\"]\ncommand = \"old\"\n"},
		{"indented header", "  [mcp_servers.brigada]\n  command = \"old\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			tc.old += "\n[other]\nkey = 1\n"
			wireWrite(t, root, wiringCodexFile, tc.old)
			if f := codexApply(t, root)[0]; f.err != nil || f.state != "replaced" {
				t.Fatalf("state %s err %v", f.state, f.err)
			}
			indent := ""
			header := "[mcp_servers.brigada]"
			if tc.name == "indented header" {
				indent, header = "  ", "  [mcp_servers.brigada]"
			}
			if tc.name == "quoted header" {
				header = "[mcp_servers.\"brigada\"]"
			}
			want := header + "\n" + indent + "command = \"brigada\"\n" + indent + "args = [\"mcp\", \"--foreman\"]\n" +
				"\n[other]\nkey = 1\n"
			if got := wireRead(t, root, wiringCodexFile); got != want {
				t.Fatalf("file:\n%q\nwant:\n%q", got, want)
			}
		})
	}
}

func TestCodexWiringDamagedFilesAreReportedAndLeftAlone(t *testing.T) {
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
			files := planCodexWiring(root)
			f := files[0]
			if f.err == nil || f.changed() {
				t.Fatalf("not reported: state %s err %v", f.state, f.err)
			}
			if !strings.Contains(f.err.Error(), "rerun init") {
				t.Errorf("error not actionable: %v", f.err)
			}
			if err := applyWiring(files); err != nil {
				t.Fatal(err)
			}
			if got := wireRead(t, root, wiringCodexFile); got != tc.body {
				t.Fatalf("damaged file rewritten: %q", got)
			}
			// Removal refuses the same damage instead of guessing.
			removal := planRemoval(root, false)
			for _, r := range removal {
				if r.rel == wiringCodexFile && (r.err == nil || r.changed()) {
					t.Errorf("remove not refused: state %s err %v", r.state, r.err)
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
		codexApply(t, root)
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
			codexApply(t, root)
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
