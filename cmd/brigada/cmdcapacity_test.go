package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// fakeCapacity stubs the capacity client: the projects it knows, the
// snapshot it serves, and the project ids the snapshot was asked for.
type fakeCapacity struct {
	projects []engine.ProjectInfo
	err      error
	asked    []int64
}

func (f *fakeCapacity) Capacity(_ context.Context, projectID int64) (engine.CapacitySnapshot, error) {
	f.asked = append(f.asked, projectID)
	if f.err != nil {
		return engine.CapacitySnapshot{}, f.err
	}
	return engine.CapacitySnapshot{
		Agents: []engine.CapacityAgent{{Harness: "claude", Model: "sonnet", Tier: "standard", Effort: "default", Cost: 1, CapLayer: "none", Quota: "ready"}},
		Layers: []engine.CapacityLayer{{Layer: "global"}},
	}, nil
}

func (f *fakeCapacity) Projects(context.Context) ([]engine.ProjectInfo, error) {
	return f.projects, nil
}

func TestPrintCapacityNamedProject(t *testing.T) {
	const repo = "https://example.invalid/capacity.git"
	fake := &fakeCapacity{projects: []engine.ProjectInfo{{ProjectID: 3, RepoURL: repo}}}
	var stdout, stderr bytes.Buffer
	if code := printCapacity(context.Background(), fake, repo, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if len(fake.asked) != 1 || fake.asked[0] != 3 {
		t.Errorf("the snapshot was asked for projects %v, want [3]", fake.asked)
	}
	out := stdout.String()
	for _, want := range []string{"AGENTS", "LAYERS", "claude/sonnet", "global"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("a buffer is not a terminal; the output has escapes:\n%q", out)
	}
}

func TestPrintCapacityUnknownProject(t *testing.T) {
	fake := &fakeCapacity{}
	var stdout, stderr bytes.Buffer
	if code := printCapacity(context.Background(), fake, "https://example.invalid/missing.git", &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no active project for repo") {
		t.Errorf("stderr = %q, want the no-project error", stderr.String())
	}
	if len(fake.asked) != 0 {
		t.Errorf("the snapshot was read for an unknown project: %v", fake.asked)
	}
}

func TestPrintCapacityReadError(t *testing.T) {
	fake := &fakeCapacity{err: errors.New("engine gone")}
	var stdout, stderr bytes.Buffer
	if code := printCapacity(context.Background(), fake, "", &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "engine gone") {
		t.Errorf("stderr = %q, want the read error", stderr.String())
	}
}

func TestCapacityTakesNoArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"capacity", "extra"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit %d, want the usage exit 2; stderr %q", code, stderr.String())
	}
}
