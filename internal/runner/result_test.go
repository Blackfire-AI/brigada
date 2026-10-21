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
		// A fence string inside a JSON string is no fence (issue #27): the
		// block is read whole, not cut at the quoted fence.
		{
			"summary quoting a backtick fence",
			"```json\n{\"status\":\"done\",\"summary\":\"wrap it in ``` fences\"}\n```\n",
			WorkerResult{Status: "done", Summary: "wrap it in ``` fences"},
		},
		{
			"summary quoting a tilde fence",
			"```json\n{\"status\":\"done\",\"summary\":\"or in ~~~ fences\"}\n```\n",
			WorkerResult{Status: "done", Summary: "or in ~~~ fences"},
		},
		{
			"summary holding a whole fenced example",
			"```json\n{\"status\":\"done\",\"summary\":\"run:\\n```sh\\ngo test ./...\\n```\\nthen push\"}\n```\n",
			WorkerResult{Status: "done", Summary: "run:\n```sh\ngo test ./...\n```\nthen push"},
		},
		{
			"job 23: fenced block whose summary quotes a fence, after prose",
			"Fixed the parser.\n\n```json\n{\"status\":\"done\",\"summary\":\"the runner read a ``` inside the summary as the fence\",\"branch\":\"brigada/task-11\"}\n```\n",
			WorkerResult{Status: "done", Summary: "the runner read a ``` inside the summary as the fence", Branch: "brigada/task-11"},
		},
		{
			"four-backtick fence",
			"````json\n{\"status\":\"done\",\"summary\":\"```\"}\n````\n",
			WorkerResult{Status: "done", Summary: "```"},
		},
		{
			"tilde fence",
			"~~~json\n{\"status\":\"done\",\"summary\":\"tilde\"}\n~~~\n",
			WorkerResult{Status: "done", Summary: "tilde"},
		},
		{
			"indented fence",
			"  ```json\n  {\"status\":\"done\",\"summary\":\"indented\"}\n  ```\n",
			WorkerResult{Status: "done", Summary: "indented"},
		},
		{
			"result followed by an unrelated fenced block",
			"```json\n{\"status\":\"done\",\"summary\":\"see the example\"}\n```\nExample:\n```go\nfmt.Println(\"hi\")\n```\n",
			WorkerResult{Status: "done", Summary: "see the example"},
		},
		{
			"fence opened mid-line after prose",
			"all done. ```json\n{\"status\":\"done\",\"summary\":\"mid-line\"}\n```",
			WorkerResult{Status: "done", Summary: "mid-line"},
		},
		{
			"fence opened mid-line, closed mid-line, words after",
			"all done. ```json\n{\"status\":\"done\",\"summary\":\"mid-line\"}\n``` that is all.",
			WorkerResult{Status: "done", Summary: "mid-line"},
		},
		{
			"unfenced block before a stray closing fence",
			"{\"status\":\"done\",\"summary\":\"stray\"}\n```\n",
			WorkerResult{Status: "done", Summary: "stray"},
		},
		{
			"windows line endings",
			"```json\r\n{\"status\":\"done\",\"summary\":\"crlf\"}\r\n```\r\n",
			WorkerResult{Status: "done", Summary: "crlf"},
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

// TestParseResultDiscoveries checks the optional discoveries field
// (§5.4): kept when the block carries it, empty when it does not.
func TestParseResultDiscoveries(t *testing.T) {
	with := parseResult("```json\n{\"status\":\"done\",\"summary\":\"s\",\"discoveries\":\"the API rounds down\"}\n```\n")
	if with.Discoveries != "the API rounds down" {
		t.Errorf("Discoveries = %q, want the worker's words", with.Discoveries)
	}
	without := parseResult("```json\n{\"status\":\"done\",\"summary\":\"s\"}\n```\n")
	if without.Discoveries != "" {
		t.Errorf("Discoveries = %q, want empty", without.Discoveries)
	}
}

// TestParseResultCommitMessage checks the optional commit_message field
// (§5.4): kept whole, newlines and all, when the block carries it,
// empty when it does not.
func TestParseResultCommitMessage(t *testing.T) {
	with := parseResult("```json\n{\"status\":\"done\",\"summary\":\"s\",\"commit_message\":\"feat(x): add a\\n\\nWrites a.txt.\\nCo-Authored-By: evil <e@x>\"}\n```\n")
	if want := "feat(x): add a\n\nWrites a.txt.\nCo-Authored-By: evil <e@x>"; with.CommitMessage != want {
		t.Errorf("CommitMessage = %q, want %q", with.CommitMessage, want)
	}
	without := parseResult("```json\n{\"status\":\"done\",\"summary\":\"s\"}\n```\n")
	if without.CommitMessage != "" {
		t.Errorf("CommitMessage = %q, want empty", without.CommitMessage)
	}
}

// TestFencedBlocks checks the CommonMark line rules fencedBlocks reads
// fences by: fences are whole lines, a closing fence matches its opener's
// character and is at least as long, and a fence string mid-line is text.
func TestFencedBlocks(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"no fence", "plain words", nil},
		{"one block", "```json\n{\"a\":1}\n```\n", []string{`{"a":1}`}},
		{"two blocks in order", "```\nfirst\n```\ntext\n~~~\nsecond\n~~~\n", []string{"first", "second"}},
		{"fence mid-line is text", "words ```json\n{\"a\":1}\n```\n", nil},
		{"fence inside the content is content", "```json\n{\"a\":\"```\"}\n```\n", []string{`{"a":"` + "```" + `"}`}},
		{"short closing fence does not close a longer opener", "````\n```\nstill inside\n````\n", []string{"```\nstill inside"}},
		{"tilde does not close a backtick fence", "```\n~~~\nstill inside\n```\n", []string{"~~~\nstill inside"}},
		{"closing fence with words is content", "```\n``` not closed\nx\n```\n", []string{"``` not closed\nx"}},
		{"indent up to three spaces", "   ```\n   x\n   ```\n", []string{"x"}},
		{"four spaces of indent is code, not a fence", "    ```\nx\n", nil},
		// The first line is inline code, so the later fence opens a block
		// that runs to the end, not closes one.
		{"backtick in a backtick fence's info string is inline code", "```a`b\nx\n```\ny\n", []string{"y"}},
		{"unclosed block runs to the end", "```json\n{\"a\":1}", []string{`{"a":1}`}},
		{"empty block is dropped", "```\n```\n", nil},
	}
	for _, tc := range cases {
		if got := fencedBlocks(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: fencedBlocks = %q, want %q", tc.name, got, tc.want)
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
