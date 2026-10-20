package engine

import (
	"database/sql"
	"testing"
)

// TestRoundCapsShippedDefault pins the shipped caps: two fix, two review
// and three restarts, the third being the foreman's after the ladder (§8).
func TestRoundCapsShippedDefault(t *testing.T) {
	want := RoundCaps{Fix: 2, Review: 2, Restart: 3}
	if got := DefaultConfig().RoundCaps; got != want {
		t.Errorf("DefaultConfig round caps = %+v, want %+v", got, want)
	}
	e := newEnvCfg(t, nil)
	if got := e.eng.cfg.RoundCaps; got != want {
		t.Errorf("engine round caps = %+v, want %+v", got, want)
	}
}

// TestExactRoundCapsAreUsed: caps the config file names reach the engine,
// a zero included, and the restart limit a task reads is the configured one.
func TestExactRoundCapsAreUsed(t *testing.T) {
	want := RoundCaps{Fix: 2, Review: 0, Restart: 5}
	e := newEnvCfg(t, func(c *Config) { c.ExactRoundCaps = &want })
	if got := e.eng.cfg.RoundCaps; got != want {
		t.Fatalf("engine round caps = %+v, want %+v", got, want)
	}
	var got int
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		got, err = e.eng.roundLimitTx(tx, e.task, roundRestart)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Errorf("restart limit = %d, want the configured 5", got)
	}
}
