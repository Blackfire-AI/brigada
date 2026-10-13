package runner

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCompactTranscriptKeepsHeadAndTail pins the shape of an over-cap
// transcript (§9): the run's opening and its end survive, the middle is
// dropped with a marker naming how much, and nothing over the cap is
// kept — the run that blew the cap keeps a record, not a refusal.
func TestCompactTranscriptKeepsHeadAndTail(t *testing.T) {
	small := []byte("one line\nanother line\n")
	if got := compactTranscript(small, 1024); string(got) != string(small) {
		t.Errorf("an under-cap transcript = %q, want it untouched", got)
	}

	buf := []byte(strings.Repeat("h", 3000) + strings.Repeat("m", 5000) + strings.Repeat("t", 3000))
	const cap = 4096
	got := string(compactTranscript(buf, cap))
	if len(got) > cap {
		t.Errorf("the compacted transcript = %d bytes, want at most the cap %d", len(got), cap)
	}
	if !strings.HasPrefix(got, "hhhh") {
		t.Errorf("the compacted transcript does not open with the head:\n%.80q", got)
	}
	if !strings.HasSuffix(got, "tttt") {
		t.Errorf("the compacted transcript does not end with the tail:%.80q", got)
	}
	if strings.Contains(got, "m") {
		t.Errorf("the compacted transcript kept middle bytes; only the head and tail survive")
	}
	if !strings.Contains(got, "bytes dropped") {
		t.Errorf("the compacted transcript names no dropped count:\n%s", got)
	}
}

// TestCompactTranscriptCutsOnRuneBoundaries checks the compaction against
// multi-byte text: a cut never leaves half a rune at either edge of the
// kept head or tail.
func TestCompactTranscriptCutsOnRuneBoundaries(t *testing.T) {
	line := "élévation ↗ json\n"
	var buf []byte
	for len(buf) < 8192 {
		buf = append(buf, line...)
	}
	got := string(compactTranscript(buf, 1024))
	if !utf8.ValidString(got) {
		t.Errorf("the compacted transcript carries a broken rune:\n%q", got)
	}
	if !strings.Contains(got, "bytes dropped") {
		t.Errorf("an all-rune transcript over the cap was not compacted:\n%.80q", got)
	}
}
