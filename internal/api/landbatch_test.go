package api

import (
	"encoding/json"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestLandBatchGrantCarriesItsMembers: a batch land grant reaches the
// runner with its mark and every member in batch order (§12.3).
func TestLandBatchGrantCarriesItsMembers(t *testing.T) {
	members := []engine.LandMember{
		{TaskID: 3, PlanID: 1, Branch: "brigada/task-3", BaseRef: "main", Message: &engine.LandMessage{Title: "a"}},
		{TaskID: 5, PlanID: 2, Branch: "brigada/task-5", BaseRef: "brigada/task-3", Deps: []int64{3}, Message: &engine.LandMessage{Title: "b"}},
	}
	b, err := json.Marshal(jobGrantOf(&engine.LeaseGrant{Kind: engine.KindLand, LandBatch: true, LandMembers: members}))
	if err != nil {
		t.Fatal(err)
	}
	var g JobGrant
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if !g.LandBatch || len(g.LandMembers) != 2 || g.LandMembers[1].TaskID != 5 || g.LandMembers[1].Deps[0] != 3 ||
		g.LandMembers[0].Message == nil || g.LandMembers[0].Message.Title != "a" {
		t.Errorf("batch grant = %+v, want its mark and both members in order", g)
	}
}

// TestLandBatchGrantCarriesGateReleases: a member's released gate and a
// re-check's mark reach the runner (§7.1 "Deciding a batch hold").
func TestLandBatchGrantCarriesGateReleases(t *testing.T) {
	members := []engine.LandMember{{TaskID: 3, Branch: "brigada/task-3", BaseRef: "main", SkipLandReady: true}}
	for _, lg := range []*engine.LeaseGrant{
		{Kind: engine.KindLand, LandBatch: true, LandMembers: members},
		{Kind: engine.KindLand, LandRecheck: true},
	} {
		b, err := json.Marshal(jobGrantOf(lg))
		if err != nil {
			t.Fatal(err)
		}
		var g JobGrant
		if err := json.Unmarshal(b, &g); err != nil {
			t.Fatal(err)
		}
		if g.LandRecheck != lg.LandRecheck || (lg.LandBatch && !g.LandMembers[0].SkipLandReady) {
			t.Errorf("grant = %+v, want the re-check mark %v and every member's release", g, lg.LandRecheck)
		}
	}
}
