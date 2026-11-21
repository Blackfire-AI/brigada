package api

import (
	"slices"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

func TestSweepGrantCarriesTheDeletableTaskIDs(t *testing.T) {
	g := jobGrantOf(&engine.LeaseGrant{Kind: engine.KindSweep, RepoURL: "https://example.invalid/r.git", SweepTaskIDs: []int64{3, 8}})
	if g.Kind != "sweep" || g.RepoURL == "" || !slices.Equal(g.SweepTaskIDs, []int64{3, 8}) {
		t.Errorf("sweep grant = %+v, want the repo and exact deletable ids", g)
	}
}
