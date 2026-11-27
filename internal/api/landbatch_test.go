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
