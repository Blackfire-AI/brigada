package runner

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// The worker result contract (§5.4): the worker's final message ends
// with one JSON block carrying the run's outcome. The statuses the
// worker may speak; the runner adds check_failed when its own check of
// a done run fails (§5.5), no-result when the block is missing or
// malformed, and stalled / time_cap when its own policy (§5.6) cut the
// run short. A worker cannot speak those itself — decodeResult refuses
// them — they are the runner's to say.
const (
	statusDone      = "done"
	statusNeedsHelp = "needs_help"
	statusFailed    = "failed"
	statusCheckFail = "check_failed"
	statusStalled   = "stalled"
	statusTimeCap   = "time_cap"
	reasonNoResult  = "no-result"
)

// WorkerResult is the parsed result block of a worker's final message
// (§5.4): what the job ended saying. The runner completes the job with
// this as the result, adding fields of its own (reason) when the
// contract itself failed.
type WorkerResult struct {
	Status   string `json:"status"`
	Summary  string `json:"summary,omitempty"`
	Question string `json:"question,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Discoveries is what the worker learned that matters beyond its own
	// task (§5.4); the runner keeps it so the engine can hand it to the
	// foreman (§4.2).
	Discoveries string `json:"discoveries,omitempty"`
	// Progress is the last progress events, attached when the runner
	// ends a stalled or time-capped run (§5.6) so what the worker was
	// doing rides with the outcome. A worker's own block never carries
	// it.
	Progress []string `json:"progress,omitempty"`
}

// parseResult reads the result block out of a worker's final message
// (§5.4). The block is the last fenced ```json block of the message; a
// message with no fence falls back to a JSON object that ends the
// message. A missing, malformed or unrecognized block is a failure with
// reason no-result, its summary carrying the tail of the final message
// so the record names what the worker actually said.
func parseResult(finalText string) WorkerResult {
	if block, ok := lastFencedJSON(finalText); ok {
		if res, ok := decodeResult(block); ok {
			return res
		}
	}
	if tail, ok := trailingJSON(finalText); ok {
		if res, ok := decodeResult(tail); ok {
			return res
		}
	}
	return WorkerResult{
		Status:  statusFailed,
		Reason:  reasonNoResult,
		Summary: tailOf(finalText, 400),
	}
}

// decodeResult turns one JSON candidate into a WorkerResult. Words may
// trail the object inside the fence — the block is there — so one JSON
// value is read off the front. An unknown status is a malformed block:
// the contract names three statuses (§5.4) and a worker that invents a
// fourth said nothing the engine can act on.
func decodeResult(block string) (WorkerResult, bool) {
	var res WorkerResult
	dec := json.NewDecoder(strings.NewReader(block))
	if err := dec.Decode(&res); err != nil {
		return WorkerResult{}, false
	}
	switch res.Status {
	case statusDone, statusNeedsHelp, statusFailed:
		return res, true
	default:
		return WorkerResult{}, false
	}
}

// lastFencedJSON returns the content of the last fenced code block in
// text, whatever language tag it carries. The result block arrives
// fenced (§5.4); the language tag is the worker's choice.
func lastFencedJSON(text string) (string, bool) {
	idx := strings.LastIndex(text, "```")
	if idx <= 0 {
		return "", false
	}
	// Walk back to the fence's opening line: the last fence before the
	// closing one. Between the two, the content sits whole.
	open := strings.LastIndex(text[:idx], "```")
	if open < 0 {
		return "", false
	}
	content := text[open+3 : idx]
	// Drop the opening fence's language tag, if any.
	if nl := strings.IndexByte(content, '\n'); nl >= 0 && !strings.Contains(content[:nl], " ") {
		content = content[nl+1:]
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "", false
	}
	return content, true
}

// trailingJSON returns the last JSON object that ends the message, so a
// worker that prints its result block unfenced still counts. The
// rightmost '{' whose remainder is one whole, valid JSON value is the
// candidate; anything after it means the message did not end with a
// block.
func trailingJSON(text string) (string, bool) {
	trimmed := strings.TrimRight(text, " \t\r\n")
	for i := strings.LastIndex(trimmed, "{"); i >= 0; i = strings.LastIndex(trimmed[:i], "{") {
		if json.Valid([]byte(trimmed[i:])) {
			return trimmed[i:], true
		}
	}
	return "", false
}

// tailOf returns the last max bytes of s, cut to a complete rune.
func tailOf(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}
