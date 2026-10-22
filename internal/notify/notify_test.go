package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// hostile text: quotes both ways, a shell/AppleScript-looking fragment,
// a newline and a tab. It must travel as data — whole argv elements —
// never merged into the script source or a command line.
var hostileTitle = "plan #2 done: \"it's `rm -rf /`\"\nsecond line"
var hostileBody = "task #3 needs you: say 'hi' \"again\"\n\t<script>alert(1)</script> $HOME"

func TestOsascriptArgsGolden(t *testing.T) {
	got := osascriptArgs(hostileTitle, hostileBody)
	want := []string{
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"--", hostileTitle, hostileBody,
	}
	if len(got) != len(want) {
		t.Fatalf("osascriptArgs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q (whole argv: %q)", i, got[i], want[i], got)
		}
	}
	// The title and body must appear exactly once each, after the `--`
	// separator — never inside a -e script line.
	dash := 0
	for i, a := range got {
		if a == "--" {
			dash = i
		}
	}
	if dash == 0 || got[dash+1] != hostileTitle || got[dash+2] != hostileBody {
		t.Errorf("title/body do not follow `--` as whole arguments: %q", got)
	}
	for _, a := range got[:dash] {
		if strings.Contains(a, hostileTitle) || strings.Contains(a, hostileBody) ||
			strings.Contains(a, "rm -rf") || strings.Contains(a, "alert(1)") {
			t.Errorf("script source %q interpolates notification text", a)
		}
	}
}

func TestNotifySendArgsGolden(t *testing.T) {
	got := notifySendArgs(hostileTitle, hostileBody)
	want := []string{"--", hostileTitle, hostileBody}
	if len(got) != len(want) {
		t.Fatalf("notifySendArgs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDetect(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/x", nil }
	missing := func(string) (string, error) { return "", errors.New("not on PATH") }

	cases := []struct {
		name     string
		goos     string
		lookPath func(string) (string, error)
		want     Notifier
	}{
		{"darwin with osascript", "darwin", found, Osascript{}},
		{"darwin without", "darwin", missing, None{}},
		{"linux with notify-send", "linux", found, NotifySend{}},
		{"linux without", "linux", missing, None{}},
		{"other OS", "windows", found, None{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := detect(tc.goos, tc.lookPath)
			switch tc.want.(type) {
			case Osascript:
				if _, ok := n.(Osascript); !ok {
					t.Fatalf("detect = %T, want Osascript", n)
				}
			case NotifySend:
				if _, ok := n.(NotifySend); !ok {
					t.Fatalf("detect = %T, want NotifySend", n)
				}
			case None:
				if _, ok := n.(None); !ok {
					t.Fatalf("detect = %T, want None", n)
				}
			}
		})
	}
}

func TestResolve(t *testing.T) {
	if _, ok := Resolve(ModeNone).(None); !ok {
		t.Errorf("Resolve(none) = %T, want None", Resolve(ModeNone))
	}
	// auto resolves to whatever Detect finds — on this host that is one
	// of the three, and it must have a Name for the startup log line.
	if n := Resolve(""); NameOf(n) == "" {
		t.Errorf("Resolve(\"\") has no name")
	}
	if n := Resolve(ModeAuto); NameOf(n) == "" {
		t.Errorf("Resolve(auto) has no name")
	}
}

func TestNoneAndFake(t *testing.T) {
	if err := (None{}).Notify(context.Background(), "t", "b"); err != nil {
		t.Errorf("None.Notify = %v, want nil", err)
	}
	f := &Fake{}
	if err := f.Notify(context.Background(), "one", "1"); err != nil {
		t.Fatal(err)
	}
	f.Err = errors.New("boom")
	if err := f.Notify(context.Background(), "two", "2"); err == nil {
		t.Errorf("Fake with Err = nil, want the error")
	}
	notes := f.Notes()
	if len(notes) != 2 || notes[0] != (Note{"one", "1"}) || notes[1] != (Note{"two", "2"}) {
		t.Errorf("Fake recorded %v, want both notifications in order", notes)
	}
}
