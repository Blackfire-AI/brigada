package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestInitPlanReviewsLine: the Reviews line of one family states the
// effective review.family policy.
func TestInitPlanReviewsLine(t *testing.T) {
	agents := []agentSpec{{harness: "claude", model: "sonnet", classes: []string{"implement", "review", "answer"}}}
	for _, tc := range []struct {
		name    string
		require bool
		want    string
	}{
		{"prefer", false, "reviews prefer another family and fall back to the same family or the same agent"},
		{"require", true, "review.family=require, so reviews wait for another family"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printInitAgents(&out, agents, 1, tc.require, agents[0], nil, adapter.AgentTier)
			flat := strings.Join(strings.Fields(out.String()), " ")
			if !strings.Contains(flat, tc.want) {
				t.Errorf("Reviews line lacks %q:\n%s", tc.want, out.String())
			}
		})
	}
}

// TestInitOneFamilyByReviewFamily: with one review family init is ready under
// review.family=prefer (the default), with a warning naming the fallback and
// no acknowledgement; under require it stops without --accept-limited.
func TestInitOneFamilyByReviewFamily(t *testing.T) {
	oneFamily := []string{"--probe", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude"}
	for _, tc := range []struct {
		name     string
		config   string
		extra    []string
		wantCode int
		want     []string
		notWant  []string
	}{
		{"prefer default", "", nil, 0, []string{"WARNING: fewer than two", "review.family=prefer", "same family or the author's own agent"}, []string{"LIMITED SETUP", "--accept-limited"}},
		{"prefer explicit", "review.family = prefer\n", nil, 0, []string{"review.family=prefer"}, []string{"LIMITED SETUP"}},
		{"require stops", "review.family = require\n", nil, 1, []string{"LIMITED SETUP", "review.family=require", "--accept-limited"}, nil},
		{"require acknowledged", "review.family = require\n", []string{"--accept-limited"}, 0, []string{"LIMITED SETUP", "review.family=require"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			if err := os.WriteFile("go.mod", []byte("module example.invalid/test\n"), 0644); err != nil {
				t.Fatal(err)
			}
			fakeInitPATH(t, "claude")
			args := append([]string{}, oneFamily...)
			args = append(args, tc.extra...)
			if tc.config != "" {
				cfg := filepath.Join(t.TempDir(), "config")
				if err := os.WriteFile(cfg, []byte(tc.config), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--config", cfg)
			}
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			code, out, errOut := initFixedParallel(t, state, "", args...)
			all := strings.Join(strings.Fields(out+errOut), " ")
			if code != tc.wantCode {
				t.Fatalf("got %d, want %d\n%s\n%s", code, tc.wantCode, out, errOut)
			}
			for _, want := range tc.want {
				if !strings.Contains(all, want) {
					t.Errorf("output lacks %q:\n%s\n%s", want, out, errOut)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(all, bad) {
					t.Errorf("output has %q:\n%s\n%s", bad, out, errOut)
				}
			}
		})
	}
}

// requireArgs expands the "@require" marker of a test's args into a
// --config naming a file that sets review.family = require.
func requireArgs(t *testing.T, args []string) []string {
	t.Helper()
	var out []string
	for _, arg := range args {
		if arg != "@require" {
			out = append(out, arg)
			continue
		}
		cfg := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(cfg, []byte("review.family = require\n"), 0600); err != nil {
			t.Fatal(err)
		}
		out = append(out, "--config", cfg)
	}
	return out
}

// TestInitIntroTextsByReviewFamily: the introduction and the workers step
// state the effective policy.
func TestInitIntroTextsByReviewFamily(t *testing.T) {
	for _, tc := range []struct {
		require bool
		want    string
	}{
		{false, "another family, if any, reviews it"},
		{true, "review.family=require"},
	} {
		if got := initIntroReviews(tc.require); !strings.Contains(got, tc.want) {
			t.Errorf("intro(require=%v) = %q, want %q", tc.require, got, tc.want)
		}
	}
	if got := initWorkersReviews(false); !strings.Contains(got, "review.family=prefer") || !strings.Contains(got, "fall back") {
		t.Errorf("workers prefer text: %q", got)
	}
	if got := initWorkersReviews(true); !strings.Contains(got, "at least two families") {
		t.Errorf("workers require text: %q", got)
	}
}
