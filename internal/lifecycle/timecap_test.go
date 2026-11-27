package lifecycle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/runner"
)

func intp(n int) *int { return &n }

func TestConfigFileTimeCaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	load := func(body string) (FileConfig, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadConfigFile(path)
	}

	// Each legal value parses into the field it names; a file that names
	// none keeps the zero config.
	for body, want := range map[string]TimeCapConfig{
		"notify = none\n":                                          {},
		"time_cap_minutes.work = 60\n":                             {Work: intp(60)},
		"time_cap_minutes.fix = 30\n":                              {Fix: intp(30)},
		"time_cap_minutes.review = 10\n":                           {Review: intp(10)},
		"time_cap_minutes.plan_review = 15\n":                      {PlanReview: intp(15)},
		"time_cap_minutes.foreman = 20\n":                          {Foreman: intp(20)},
		"time_cap_minutes.land = 25\n":                             {Land: intp(25)},
		"time_cap_minutes.work = 60\ntime_cap_minutes.land = 25\n": {Work: intp(60), Land: intp(25)},
	} {
		cfg, err := load(body)
		if err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if got := cfg.TimeCaps; !reflect.DeepEqual(got, want) {
			t.Errorf("%q: time caps = %+v, want %+v", body, got, want)
		}
	}

	// An illegal value or an unknown kind is refused, naming the line.
	for name, body := range map[string]string{
		"zero":         "time_cap_minutes.work = 0\n",
		"negative":     "time_cap_minutes.work = -5\n",
		"not a number": "time_cap_minutes.work = many\n",
		"fraction":     "time_cap_minutes.work = 1.5\n",
		"unknown kind": "time_cap_minutes.audit = 5\n",
		"sweep kind":   "time_cap_minutes.sweep = 5\n",
		"sync kind":    "time_cap_minutes.sync = 5\n",
	} {
		if _, err := load(body); err == nil || !strings.Contains(err.Error(), "line 1") {
			t.Errorf("%s: err = %v, want a refusal naming line 1", name, err)
		}
	}

	// A refused value names the rule: positive whole minutes.
	if _, err := load("time_cap_minutes.work = 0\n"); err == nil || !strings.Contains(err.Error(), "positive whole number of minutes") {
		t.Errorf("zero refusal = %v, want the positive-minutes rule named", err)
	}
	// A refused kind names the kinds it takes, sweep among the unknowns.
	if _, err := load("time_cap_minutes.sweep = 5\n"); err == nil || !strings.Contains(err.Error(), "want work, fix, review, plan_review, foreman or land") {
		t.Errorf("sweep refusal = %v, want the valid kinds named", err)
	}
	// The unknown-key want-list names the new keys.
	if _, err := load("frobnicate = 1\n"); err == nil || !strings.Contains(err.Error(), "time_cap_minutes.<kind>") {
		t.Errorf("unknown-key refusal = %v, want the time_cap_minutes key named", err)
	}
}

func TestTimeCapConfigDerivations(t *testing.T) {
	// Unset keeps the shipped behavior: the runner's class caps, and the
	// engine's hard caps one 5-minute margin above them — the same table
	// the engine ships as its own default.
	def := TimeCapConfig{}
	if got := def.RunnerCaps(); got != nil {
		t.Errorf("RunnerCaps() = %+v, want nil when the file names none", got)
	}
	wantClass := map[engine.Kind]time.Duration{
		engine.KindWork:       40 * time.Minute,
		engine.KindFix:        40 * time.Minute,
		engine.KindReview:     20 * time.Minute,
		engine.KindPlanReview: 20 * time.Minute,
		engine.KindForeman:    25 * time.Minute,
	}
	if got := def.ClassCaps(); !reflect.DeepEqual(got, wantClass) {
		t.Errorf("ClassCaps() = %+v, want %+v", got, wantClass)
	}
	if got := def.HardCaps(); !reflect.DeepEqual(got, engine.DefaultConfig().HardCaps) {
		t.Errorf("unset HardCaps() = %+v, want the engine defaults %+v", got, engine.DefaultConfig().HardCaps)
	}

	// A named kind overrides both, preserving the +5m margin; land gains
	// a cap only because the file names one.
	cfg := TimeCapConfig{Work: intp(60), Land: intp(30)}
	wantClass = map[engine.Kind]time.Duration{
		engine.KindWork:       60 * time.Minute,
		engine.KindFix:        40 * time.Minute,
		engine.KindReview:     20 * time.Minute,
		engine.KindPlanReview: 20 * time.Minute,
		engine.KindForeman:    25 * time.Minute,
		engine.KindLand:       30 * time.Minute,
	}
	if got := cfg.ClassCaps(); !reflect.DeepEqual(got, wantClass) {
		t.Errorf("ClassCaps() = %+v, want %+v", got, wantClass)
	}
	// The margin invariant: every hard cap is its class cap plus 5 minutes.
	class := cfg.ClassCaps()
	hard := cfg.HardCaps()
	if len(hard) != len(class) {
		t.Errorf("HardCaps() has %d kinds, ClassCaps() %d", len(hard), len(class))
	}
	for kind, cap := range class {
		if hard[kind] != cap+5*time.Minute {
			t.Errorf("hard cap %s = %s, want class cap %s plus 5 minutes", kind, hard[kind], cap)
		}
	}

	// The runner's class-caps mapping carries only the named kinds.
	wantRunner := map[string]runner.ClassCaps{
		"work": {TimeCap: 60 * time.Minute},
		"land": {TimeCap: 30 * time.Minute},
	}
	if got := cfg.RunnerCaps(); !reflect.DeepEqual(got, wantRunner) {
		t.Errorf("RunnerCaps() = %+v, want %+v", got, wantRunner)
	}
}
