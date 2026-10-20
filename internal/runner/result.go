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
	// ExtraRepos is the commit each extra repository resolved to (§5.8),
	// by name, so the history shows what the job saw. The runner sets it;
	// a worker's own block never carries it.
	ExtraRepos map[string]string `json:"extra_repos,omitempty"`
	// Head is the commit the runner pushed as the task's branch (§7), so
	// a later re-review can ask for the fix round's diff alone. The
	// runner sets it; a worker's own block never carries it.
	Head string `json:"head,omitempty"`
}

// parseResult reads the result block out of a worker's final message
// (§5.4). The block is the last fenced ```json block of the message; the
// candidates come from blockCandidates, in its order — fenced blocks by
// the line rules, last first, then a JSON object ending the message, then
// the old substring heuristic — and the first that decodes as a result
// wins, so a summary quoting a fence, or an unrelated example block after
// the result, no longer hides it. A missing, malformed or unrecognized
// block is a failure with reason no-result, its summary carrying the tail
// of the final message so the record names what the worker actually said.
func parseResult(finalText string) WorkerResult {
	for _, block := range blockCandidates(finalText) {
		if res, ok := decodeResult(block); ok {
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

// blockCandidates returns the texts of a final message that may be its
// JSON block, best first; the caller decodes each in turn and takes the
// first that is the block it expects. Shared by the worker result (§5.4)
// and the review verdict (§7). The attempts, in order:
//
//  1. Fenced code blocks by the CommonMark line rules (fencedBlocks),
//     last first. A fence string inside a JSON string is never a fence
//     here, so a summary quoting one no longer cuts the block in two;
//     and a message ending with an unrelated example block still yields
//     the result before it.
//  2. A JSON object ending the message, after one closing-fence line is
//     dropped from its end: the unfenced block, and the block whose
//     fence opened mid-line after prose and so was no fence by rule 1.
//  3. The substring heuristic of lastFenceSubstring, the parser before
//     the line rules, kept for whatever shape it alone still reads.
func blockCandidates(text string) []string {
	blocks := fencedBlocks(text)
	var out []string
	for i := len(blocks) - 1; i >= 0; i-- {
		out = append(out, blocks[i])
	}
	if tail, ok := trailingJSON(stripClosingFence(text)); ok {
		out = append(out, tail)
	}
	if block, ok := lastFenceSubstring(text); ok {
		out = append(out, block)
	}
	return out
}

// fencedBlocks returns the content of every fenced code block in text,
// in order, by CommonMark's rules: an opening fence is a line of at most
// three spaces of indent, then three or more backticks or tildes, then
// an optional info string (the language tag); the closing fence is a
// line of the same character, at least as long, with nothing but
// whitespace after it. A block left open runs to the end of the text.
// Content is trimmed; empty blocks are dropped.
func fencedBlocks(text string) []string {
	var blocks []string
	var openCh byte
	var openLen int
	var content []string
	inBlock := false
	flush := func() {
		if c := strings.TrimSpace(strings.Join(content, "\n")); c != "" {
			blocks = append(blocks, c)
		}
		inBlock = false
	}
	for _, line := range strings.Split(text, "\n") {
		ch, n, info, ok := fenceOf(line)
		if inBlock {
			if ok && ch == openCh && n >= openLen && strings.TrimSpace(info) == "" {
				flush()
			} else {
				content = append(content, line)
			}
			continue
		}
		// A backtick fence's info string may not hold a backtick: such a
		// line is inline code, not a fence.
		if ok && !(ch == '`' && strings.Contains(info, "`")) {
			inBlock, openCh, openLen, content = true, ch, n, nil
		}
	}
	if inBlock {
		flush()
	}
	return blocks
}

// fenceOf reads one line as a code fence: up to three spaces of indent,
// then a run of three or more backticks or tildes. It returns the fence
// character, the run's length and the rest of the line (the info string
// of an opening fence; blank on a closing one). ok is false when the
// line is not a fence.
func fenceOf(line string) (ch byte, n int, info string, ok bool) {
	line = strings.TrimRight(line, "\r")
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i >= len(line) || (line[i] != '`' && line[i] != '~') {
		return 0, 0, "", false
	}
	ch = line[i]
	j := i
	for j < len(line) && line[j] == ch {
		j++
	}
	if n = j - i; n < 3 {
		return 0, 0, "", false
	}
	return ch, n, line[j:], true
}

// stripClosingFence drops one closing-fence line from the end of text,
// trailing whitespace included, so the trailing-object fallback sees a
// message that ends with its object when the worker opened its fence
// mid-line — no fence by the line rules — and closed it on a line of its
// own. Any other ending is returned unchanged.
func stripClosingFence(text string) string {
	trimmed := strings.TrimRight(text, " \t\r\n")
	i := strings.LastIndexByte(trimmed, '\n')
	if _, _, info, ok := fenceOf(trimmed[i+1:]); !ok || strings.TrimSpace(info) != "" {
		return text
	}
	if i < 0 {
		return ""
	}
	return trimmed[:i]
}

// lastFenceSubstring returns what sits between the last two triple
// backticks of text, whatever language tag the first carries. The
// parser before the line rules, kept as the last resort: it reads a
// fence string anywhere — inside a JSON string too, which is the failure
// the line rules fix — so it only speaks after they and the trailing
// object both found nothing.
func lastFenceSubstring(text string) (string, bool) {
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
