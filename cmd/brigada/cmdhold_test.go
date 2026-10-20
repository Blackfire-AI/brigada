package main

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// The old hooks run `hold yield` and `hold confirm` and may pipe a payload
// on stdin; both must exit 0 with no output either way.
func TestKeptHoldIsNoOp(t *testing.T) {
	payloads := map[string]string{"without stdin": "", "with stdin": `{"session_id":"x"}`}
	for _, args := range [][]string{{"hold", "yield"}, {"hold", "confirm"}, {"hold", "yield", "--mine"}} {
		for name, stdin := range payloads {
			t.Run(name, func(t *testing.T) {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.WriteString(stdin); err != nil {
					t.Fatal(err)
				}
				w.Close()
				saved := os.Stdin
				os.Stdin = r
				defer func() { os.Stdin = saved }()

				var out, errOut bytes.Buffer
				if code := run(context.Background(), args, &out, &errOut); code != 0 {
					t.Fatalf("run(%q) code = %d, want 0", args, code)
				}
				if out.Len() != 0 || errOut.Len() != 0 {
					t.Errorf("run(%q) printed stdout=%q stderr=%q, want nothing", args, out.String(), errOut.String())
				}
			})
		}
	}
}

// Any other hold subcommand, release included, stays a usage error.
func TestOtherHoldIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"hold", "release"}, &out, &errOut); code != 2 {
		t.Errorf("run(hold release) code = %d, want 2", code)
	}
}
