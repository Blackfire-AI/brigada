package runner

import (
	"fmt"
	"unicode/utf8"
)

// The transcript's bounds (§9). The engine caps one artifact upload at
// 64 MiB by default, and the runner keeps to the same number: a
// transcript that outgrows it is compacted — head and tail kept, the
// middle dropped with a marker naming how much — rather than refused
// wholesale, because the run that blew the cap is exactly the run
// whose transcript is worth reading. An engine configured below the
// default can still refuse the upload; the runner cannot know its
// number, only keep to the shipped one.
const (
	// maxTranscriptBytes is the shipped bound, matching the engine's
	// upload cap default.
	maxTranscriptBytes = 64 << 20
	// transcriptHead is how much of the run's opening is kept whole
	// when the bound bites: the session init and the first events.
	transcriptHead = 1 << 20
)

// uploadCap is the run's artifact size bound: the configured one, else
// the shipped default.
func (r *jobRun) uploadCap() int {
	if r.transcriptCap > 0 {
		return r.transcriptCap
	}
	return maxTranscriptBytes
}

// markerSlack is room left for the truncation marker's text when the
// tail is sized; the marker built below fits in it for any dropped
// count a length can express.
const markerSlack = 96

// compactTranscript cuts buf to at most cap bytes when it is over:
// the head — the run's opening — and the tail — the end, where the
// result block and the last events sit — are kept, with a marker in
// between naming how many bytes were dropped. Under the cap it is
// returned as it is.
func compactTranscript(buf []byte, cap int) []byte {
	if len(buf) <= cap {
		return buf
	}
	head := transcriptHead
	if head > cap/4 {
		head = cap / 4
	}
	tailLen := cap - head - markerSlack
	if tailLen < 0 {
		tailLen = 0
	}
	dropped := len(buf) - head - tailLen
	if dropped < 0 {
		dropped = 0
	}
	marker := fmt.Sprintf("\n… %d bytes dropped to fit the artifact cap, head kept then tail …\n", dropped)
	out := make([]byte, 0, head+len(marker)+tailLen)
	out = append(out, cutHead(string(buf), head)...)
	out = append(out, marker...)
	out = append(out, cutTail(string(buf), tailLen)...)
	return out
}

// cutHead returns the first n bytes of s cut back to a complete rune.
func cutHead(s string, n int) string {
	if n >= len(s) {
		return s
	}
	s = s[:n]
	for len(s) > 0 {
		_, size := utf8.DecodeLastRuneInString(s)
		if size == 1 && s[len(s)-1] >= utf8.RuneSelf {
			s = s[:len(s)-1] // the last byte is a cut rune's continuation
			continue
		}
		break
	}
	return s
}

// cutTail returns the last n bytes of s cut forward to a complete rune.
func cutTail(s string, n int) string {
	if n >= len(s) {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && (s[0]&0xC0) == 0x80 {
		s = s[1:] // the first byte belongs to a rune whose lead was cut
	}
	return s
}
