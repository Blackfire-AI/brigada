package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"strings"
	"testing"
)

func helpOutput(t *testing.T, args ...string) string {
	t.Helper()
	var out, err bytes.Buffer
	if code := run(context.Background(), args, &out, &err); code != 0 || err.Len() != 0 {
		t.Fatalf("run(%q): code=%d stdout=%q stderr=%q", args, code, out.String(), err.String())
	}
	return out.String()
}

// Walk the production tables, so a new command is required to provide help
// and every registered flag is checked without a hand-maintained flag list.
func TestCommandHelp(t *testing.T) {
	var walk func([]string, map[string]command)
	walk = func(parent []string, table map[string]command) {
		for name, cmd := range table {
			path := append(append([]string{}, parent...), name)
			t.Run(strings.Join(path, "/"), func(t *testing.T) {
				var fs *flag.FlagSet
				factory := newFlagSet
				newFlagSet = func(name, synopsis, description string) *flag.FlagSet {
					if !strings.HasPrefix(synopsis, "brigada "+strings.Join(path, " ")) || strings.Contains(synopsis, "\n") {
						t.Errorf("missing or invalid one-line synopsis: %q", synopsis)
					}
					if strings.TrimSpace(description) == "" {
						t.Error("missing description")
					}
					fs = factory(name, synopsis, description)
					return fs
				}
				t.Cleanup(func() { newFlagSet = factory })
				out := helpOutput(t, append(append([]string{}, path...), "--help")...)
				if !strings.HasPrefix(out, "Usage: brigada "+strings.Join(path, " ")) {
					t.Fatalf("missing command usage: %q", out)
				}
				if strings.Contains(out, "§") {
					t.Errorf("help cites a design section: %q", out)
				}
				if len(cmd.subcommands) != 0 {
					for sub, child := range cmd.subcommands {
						line := fmt.Sprintf("  %-10s %s\n", sub, child.description)
						if child.description == "" || !strings.Contains(out, line) {
							t.Errorf("dispatcher help missing description for %s: %q", sub, out)
						}
					}
				} else {
					if fs == nil {
						t.Fatal("command did not declare a flag set and synopsis")
					}
					for _, common := range []string{"state-dir", "config"} {
						if fs.Lookup(common) == nil {
							t.Errorf("missing common flag %s", common)
						}
					}
					fs.VisitAll(func(f *flag.Flag) {
						if f.Usage == "" {
							t.Errorf("flag %s has no help text", f.Name)
						}
						argument, text := flag.UnquoteUsage(f)
						label := "  -" + f.Name
						if argument != "" {
							label += " " + argument
						}
						if !strings.Contains(out, label+"\n") && !strings.Contains(out, label+"\t") {
							t.Errorf("help missing flag and argument %q: %q", label, out)
						}
						if !strings.Contains(out, text) {
							t.Errorf("help missing text for %s: %q", f.Name, text)
						}
					})
				}
				aliases := []string{"-h", "--help", "-help"}
				if len(cmd.subcommands) != 0 {
					aliases = append(aliases, "help")
				}
				for _, alias := range aliases {
					if got := helpOutput(t, append(append([]string{}, path...), alias)...); got != out {
						t.Errorf("%s differs from --help", alias)
					}
				}
				if got := helpOutput(t, append([]string{"help"}, path...)...); got != out {
					t.Error("brigada help command differs from command --help")
				}
				if len(cmd.subcommands) == 0 {
					for _, alias := range aliases {
						args := append(append([]string{}, path...), "--config", "missing-config", alias, "--state-dir", "missing-state")
						if got := helpOutput(t, args...); got != out {
							t.Errorf("help among flags differs for %s", alias)
						}
					}
				}
			})
			walk(path, cmd.subcommands)
		}
	}
	walk(nil, commands)
}

// The top-level usage is for users: no design-section citations.
func TestUsageCitesNoDesignSection(t *testing.T) {
	if strings.Contains(usage, "§") {
		t.Errorf("usage cites a design section:\n%s", usage)
	}
}

func TestHelpAfterPositional(t *testing.T) {
	for _, name := range []string{"land", "diff", "logs"} {
		for _, alias := range []string{"-h", "--help", "-help"} {
			t.Run(name+"/"+alias, func(t *testing.T) {
				want := helpOutput(t, name, "--help")
				if got := helpOutput(t, name, "7", alias); got != want {
					t.Errorf("help after positional differs: %q", got)
				}
			})
		}
	}
	if got, want := helpOutput(t, "land", "7", "--no-check", "8", "--help"), helpOutput(t, "land", "--help"); got != want {
		t.Error("land help after interleaved flags and task IDs differs")
	}
}

// --and-stop-asking changes a rule only on approve; its help line must
// name no argument (a backquoted phrase would become one) and reject
// must not offer it.
func TestStopAskingHelpLine(t *testing.T) {
	for _, args := range [][]string{{"plan", "approve"}, {"land", "approve"}} {
		out := helpOutput(t, append(args, "--help")...)
		if !strings.Contains(out, "  -and-stop-asking\n") || strings.Contains(out, "-and-stop-asking brigada") {
			t.Errorf("%v help has a bad stop-asking line:\n%s", args, out)
		}
	}
	for _, args := range [][]string{{"plan", "reject"}, {"land", "reject"}} {
		if out := helpOutput(t, append(args, "--help")...); strings.Contains(out, "and-stop-asking") {
			t.Errorf("%v help lists --and-stop-asking, which it ignores:\n%s", args, out)
		}
	}
}

// A bare dispatcher prints its usage line once, with no brigada: prefix
// in front of it, then the hint.
func TestBareDispatcherUsageOnce(t *testing.T) {
	want := map[string]string{
		"plan": "usage: brigada plan approve [--and-stop-asking] <id> | brigada plan reject <id>",
		"push": "usage: brigada push approve [--repo URL] [--no-start]",
	}
	for cmd, line := range want {
		var out, err bytes.Buffer
		if code := run(context.Background(), []string{cmd}, &out, &err); code != 2 {
			t.Fatalf("%s: code=%d stderr=%q", cmd, code, err.String())
		}
		wantErr := line + "\nRun 'brigada " + cmd + " --help' for usage.\n"
		if err.String() != wantErr {
			t.Errorf("%s: stderr=%q, want %q", cmd, err.String(), wantErr)
		}
	}
}

func TestUsageErrorHelpHint(t *testing.T) {
	for _, args := range [][]string{
		{"land", "--bad-flag"}, {"init", "--bad-flag"}, {"init", "--config"},
		{"init", "extra"}, {"cancel"}, {"cancel", "seven"}, {"land"},
		{"project", "foreman", "--bad-flag"}, {"task", "submit"}, {"agent", "reset"},
		{"plan", "approve"}, {"project", "bad-subcommand"},
	} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			var out, err bytes.Buffer
			if code := run(context.Background(), args, &out, &err); code != 2 || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), err.String())
			}
			path := args[:1]
			if len(args) > 1 {
				if _, ok := commands[args[0]].subcommands[args[1]]; ok {
					path = args[:2]
				}
			}
			hint := "Run 'brigada " + strings.Join(path, " ") + " --help' for usage.\n"
			if !strings.HasSuffix(err.String(), hint) {
				t.Errorf("stderr=%q, want suffix %q", err.String(), hint)
			}
			if strings.Contains(strings.Join(args, " "), "--bad-flag") && !strings.HasPrefix(err.String(), "brigada: flag provided but not defined: -bad-flag\n") {
				t.Errorf("bad flag message changed: %q", err.String())
			}
		})
	}
}
