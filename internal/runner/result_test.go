package runner

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseResultFenced(t *testing.T) {
	msg := "I did the work.\n```json\n{\"status\":\"done\",\"summary\":\"edited work.txt\",\"branch\":\"brigade/task-1\"}\n```\n"
	got := parseResult(msg)
	want := WorkerResult{Status: "done", Summary: "edited work.txt", Branch: "brigade/task-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseResult = %+v, want %+v", got, want)
	}
}

func TestParseResultTable(t *testing.T) {
	cases := []struct {
		name string
		text string
		want WorkerResult
	}{
		{
			"unfenced trailing block",
			`all done: {"status":"done","summary":"it works"}`,
			WorkerResult{Status: "done", Summary: "it works"},
		},
		{
			"needs help with question",
			"```json\n{\"status\":\"needs_help\",\"question\":\"which database?\"}\n```",
			WorkerResult{Status: "needs_help", Question: "which database?"},
		},
		{
			"worker failed",
			"```json\n{\"status\":\"failed\",\"summary\":\"no such API\"}\n```",
			WorkerResult{Status: "failed", Summary: "no such API"},
		},
		{
			"last of two fences wins",
			"```json\n{\"status\":\"failed\"}\n```\nmore text\n```json\n{\"status\":\"done\"}\n```",
			WorkerResult{Status: "done"},
		},
		{
			"no language tag on the fence",
			"```\n{\"status\":\"done\"}\n```",
			WorkerResult{Status: "done"},
		},
		{
			"no block at all",
			"I decided not to tell you anything.",
			WorkerResult{Status: "failed", Reason: "no-result", Summary: "I decided not to tell you anything."},
		},
		{
			"malformed json",
			"```json\n{\"status\": \"done\", \n```",
			WorkerResult{Status: "failed", Reason: "no-result", Summary: "```json\n{\"status\": \"done\", \n```"},
		},
		{
			"unknown status counts as malformed",
			"```json\n{\"status\":\"finished\"}\n```",
			WorkerResult{Status: "failed", Reason: "no-result", Summary: "```json\n{\"status\":\"finished\"}\n```"},
		},
		{
			// stalled and time_cap are the runner's outcomes (§5.6), not
			// statuses a worker may speak for itself.
			"a worker cannot declare the runner's outcomes",
			"```json\n{\"status\":\"stalled\"}\n```",
			WorkerResult{Status: "failed", Reason: "no-result", Summary: "```json\n{\"status\":\"stalled\"}\n```"},
		},
		{
			"empty message",
			"",
			WorkerResult{Status: "failed", Reason: "no-result"},
		},
		{
			"nested braces in the block",
			"```json\n{\"status\":\"done\",\"summary\":\"a {b} c\"}\n```",
			WorkerResult{Status: "done", Summary: "a {b} c"},
		},
		{
			"text after the closing brace inside the fence is still parsed",
			"```json\n{\"status\":\"done\"} trailing words\n```",
			WorkerResult{Status: "done"},
		},
	}
	for _, tc := range cases {
		got := parseResult(tc.text)
		if got.Status != tc.want.Status || got.Reason != tc.want.Reason ||
			got.Summary != tc.want.Summary || got.Question != tc.want.Question || got.Branch != tc.want.Branch {
			t.Errorf("%s: parseResult = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestTailOf(t *testing.T) {
	if got := tailOf("hello", 10); got != "hello" {
		t.Errorf("short string: %q", got)
	}
	if got := tailOf("0123456789", 4); got != "6789" {
		t.Errorf("cut: %q", got)
	}
	// A cut inside a multibyte rune backs off to a whole rune: 5 bytes
	// hold only two two-byte é.
	s := strings.Repeat("é", 10)
	if got := tailOf(s, 5); got != "éé" {
		t.Errorf("rune-safe cut = %q", got)
	}
}
