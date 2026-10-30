package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

func TestConfigFileReview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	load := func(body string) (FileConfig, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadConfigFile(path)
	}
	for body, want := range map[string]engine.ReviewConfig{
		"notify = none\n":                            {},
		"review.tier = same\n":                       {Tier: "same"},
		"review.tier = below\nreview.effort = max\n": {Tier: "below", Effort: "max"},
		"review.effort = low\n":                      {Effort: "low"},
		// review.family (issue #126): absent is the prefer default;
		// require keeps the cross-family-only rule.
		"review.family = prefer\n":  {Family: "prefer"},
		"review.family = require\n": {Family: "require"},
		"review.tier = same\nreview.effort = low\nreview.family = require\n": {Tier: "same", Effort: "low", Family: "require"},
	} {
		cfg, err := load(body)
		if err != nil {
			t.Errorf("%q: %v", body, err)
		} else if cfg.Review != want {
			t.Errorf("%q: review = %+v, want %+v", body, cfg.Review, want)
		}
	}
	for name, body := range map[string]string{
		"tier word":    "review.tier = above\n",
		"empty tier":   "review.tier =\n",
		"effort word":  "review.effort = xhigh\n",
		"empty effort": "review.effort =\n",
		"family word":  "review.family = maybe\n",
		"empty family": "review.family =\n",
		"unknown key":  "review.model = x\n",
	} {
		if _, err := load(body); err == nil || !strings.Contains(err.Error(), "line 1") {
			t.Errorf("%s: err = %v, want a refusal naming line 1", name, err)
		}
	}
	// A refused review.family names the two valid values, as review.tier
	// does.
	if _, err := load("review.family = maybe\n"); err == nil || !strings.Contains(err.Error(), "want prefer or require") {
		t.Errorf("family refusal = %v, want the two valid values named", err)
	}
	if _, err := load("review.family = maybe\n"); err == nil || !strings.Contains(err.Error(), "review.family") {
		t.Errorf("family refusal = %v, want the key named", err)
	}
}

func TestConfigFileRounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	write := func(body string) FileConfig {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfigFile(path)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		return cfg
	}

	for body, want := range map[string]engine.RoundCaps{
		"notify = none\n": {Fix: 2, Review: 2, Restart: 3},
		"rounds.fix = 4\nrounds.review = 0\nrounds.restart = 5\n": {Fix: 4, Review: 0, Restart: 5},
		"rounds.restart = 1\n": {Fix: 2, Review: 2, Restart: 1},
	} {
		if got := write(body).Rounds.Caps(); got != want {
			t.Errorf("%q: caps = %+v, want %+v", body, got, want)
		}
	}

	for name, body := range map[string]string{
		"negative":    "rounds.fix = -1\n",
		"not a count": "rounds.review = many\n",
		"unknown":     "rounds.extra = 1\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfigFile(path); err == nil || !strings.Contains(err.Error(), "line 1") {
			t.Errorf("%s: err = %v, want a refusal naming line 1", name, err)
		}
	}
}
