package engine

import (
	"math"
	"math/rand"
	"testing"
)

func TestBetaDrawMean(t *testing.T) {
	const n = 40000
	for _, tc := range []struct{ a, b float64 }{
		{0.5, 0.5}, {0.5, 4.5}, {1.5, 0.5}, {4.5, 1.5}, {20.5, 0.5}, {10.5, 10.5},
	} {
		rnd := rand.New(rand.NewSource(1)).Float64
		var sum float64
		for range n {
			p := betaDraw(rnd, tc.a, tc.b)
			if p < 0 || p > 1 || math.IsNaN(p) {
				t.Fatalf("Beta(%v, %v) drew %v", tc.a, tc.b, p)
			}
			sum += p
		}
		want := tc.a / (tc.a + tc.b)
		if got := sum / n; math.Abs(got-want) > 0.01 {
			t.Errorf("Beta(%v, %v) mean = %.4f, want %.4f", tc.a, tc.b, got, want)
		}
	}
}

func TestGammaDrawMean(t *testing.T) {
	const n = 40000
	for _, shape := range []float64{0.5, 0.9, 1, 3.5} {
		rnd := rand.New(rand.NewSource(2)).Float64
		var sum float64
		for range n {
			sum += gammaDraw(rnd, shape)
		}
		if got := sum / n; math.Abs(got-shape) > 0.05*math.Max(shape, 1) {
			t.Errorf("Gamma(%v) mean = %.4f, want %v", shape, got, shape)
		}
	}
}

// A degenerate source must not hang the sampler.
func TestBetaDrawConstantSource(t *testing.T) {
	for _, u := range []float64{0, 0.5, 0.999999} {
		if p := betaDraw(func() float64 { return u }, 0.5, 20.5); p < 0 || p > 1 || math.IsNaN(p) {
			t.Errorf("constant %v drew %v", u, p)
		}
	}
}

// firstShare is the share of trials over which the agent at index 0 of
// the two ranks first.
func firstShare(t *testing.T, seed int64, mk func() []agentCand) float64 {
	t.Helper()
	rnd := rand.New(rand.NewSource(seed)).Float64
	const trials = 4000
	won := 0
	for range trials {
		cands := mk()
		rankAgents(cands, rnd)
		if cands[0].id == 1 {
			won++
		}
		for i, c := range cands {
			if c.rank != i {
				t.Fatalf("rank of %d = %d, want %d", c.id, c.rank, i)
			}
		}
	}
	return float64(won) / trials
}

func TestRankAgentsWeightSteers(t *testing.T) {
	cheap := func() []agentCand {
		return []agentCand{{id: 1, weight: 1}, {id: 2, weight: 5}}
	}
	if got := firstShare(t, 1, cheap); got < 0.65 {
		t.Errorf("weight 1 ranked first %.2f of the time against weight 5, want most", got)
	}
	equal := func() []agentCand {
		return []agentCand{{id: 1, weight: 1}, {id: 2, weight: 1}}
	}
	if got := firstShare(t, 1, equal); got < 0.45 || got > 0.55 {
		t.Errorf("equal weights and records: id 1 first %.2f of the time, want about half", got)
	}
}

func TestRankAgentsRecordBeatsSlightlyCheaper(t *testing.T) {
	mk := func() []agentCand {
		return []agentCand{
			{id: 1, weight: 3, attempts: 40, success: 38},
			{id: 2, weight: 2, attempts: 40, success: 20},
		}
	}
	if got := firstShare(t, 3, mk); got < 0.8 {
		t.Errorf("the clearly better record ranked first %.2f of the time, want most", got)
	}
}

func TestRankAgentsZeroWeight(t *testing.T) {
	// A free agent with a poor record still goes before a paid, perfect one.
	rnd := rand.New(rand.NewSource(4)).Float64
	for range 500 {
		cands := []agentCand{
			{id: 1, weight: 2, attempts: 50, success: 50},
			{id: 2, weight: 0, attempts: 50, success: 0},
		}
		rankAgents(cands, rnd)
		if cands[0].id != 2 {
			t.Fatalf("order = %d, %d, want the weight-0 agent first", cands[0].id, cands[1].id)
		}
	}
	// Among free agents the higher draw goes first.
	for range 500 {
		cands := []agentCand{
			{id: 1, weight: 0, attempts: 50, success: 0},
			{id: 2, weight: 0, attempts: 50, success: 50},
			{id: 3, weight: 1, attempts: 50, success: 50},
		}
		rankAgents(cands, rnd)
		if cands[0].id != 2 || cands[1].id != 1 || cands[2].id != 3 {
			t.Fatalf("order = %d, %d, %d, want 2, 1, 3", cands[0].id, cands[1].id, cands[2].id)
		}
		if cands[0].draw < cands[1].draw {
			t.Fatalf("free agents out of draw order: %v before %v", cands[0].draw, cands[1].draw)
		}
	}
}

// Exact ties break by the further draw, not by id.
func TestRanksBeforeTieUsesFurtherDraw(t *testing.T) {
	a := agentCand{id: 1, weight: 2, draw: 0.5, tie: 0.9}
	b := agentCand{id: 2, weight: 2, draw: 0.5, tie: 0.1}
	if ranksBefore(&a, &b) || !ranksBefore(&b, &a) {
		t.Errorf("tied agents: id 1 first, want the lower further draw first")
	}
	za := agentCand{id: 1, weight: 0, draw: 0.5, tie: 0.9}
	zb := agentCand{id: 2, weight: 0, draw: 0.5, tie: 0.1}
	if ranksBefore(&za, &zb) || !ranksBefore(&zb, &za) {
		t.Errorf("tied free agents: id 1 first, want the lower further draw first")
	}
}

// With no source the candidates keep their id order and draw nothing.
func TestRankAgentsNilSourceKeepsOrder(t *testing.T) {
	cands := []agentCand{{id: 1, weight: 5}, {id: 2, weight: 0}}
	rankAgents(cands, nil)
	if cands[0].id != 1 || cands[0].rank != 0 || cands[1].rank != 1 || cands[0].draw != 0 {
		t.Errorf("candidates = %+v, want id order, undrawn", cands)
	}
}
