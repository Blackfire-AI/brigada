package fake

import (
	"slices"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestCommandCarriesPermission(t *testing.T) {
	a := &Adapter{Script: "/bin/true"}
	cmd, err := a.Command(adapter.Job{Prompt: "p", Permission: "plan"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Args[1:], []string{"--permission", "plan"}) {
		t.Errorf("args = %v, want --permission plan", cmd.Args)
	}
	review, err := a.Command(adapter.Job{Prompt: "p", Permission: "plan", Review: true}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(review.Args[1:], []string{"--permission", "plan"}) {
		t.Errorf("review args = %v, want the same pass-through", review.Args)
	}
	none, err := a.Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Args) != 1 {
		t.Errorf("args = %v, want none without a permission", none.Args)
	}
}
