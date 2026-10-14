package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// eventRow is one log row as the assertions below read it.
type eventRow struct {
	kind    string
	job     int64
	payload eventPayload
}

// eventRows reads the log in write order, payload decoded; the payload
// of every row must be the engine's own shape.
func (e *env) eventRows() []eventRow {
	e.t.Helper()
	var out []eventRow
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT kind, COALESCE(job_id, 0), payload FROM events ORDER BY id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r eventRow
			var raw string
			if err := rows.Scan(&r.kind, &r.job, &raw); err != nil {
				return err
			}
			if json.Unmarshal([]byte(raw), &r.payload) != nil {
				e.t.Fatalf("event %s payload %q is not the engine's shape", r.kind, raw)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatalf("read events: %v", err)
	}
	return out
}

func (e *env) eventKinds() []string {
	e.t.Helper()
	var out []string
	for _, r := range e.eventRows() {
		out = append(out, r.kind)
	}
	return out
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestEventsTraceTheJobLifecycle(t *testing.T) {
	e := newEnv(t)

	// submit: the created event, then the capped and pending answers.
	r := e.submit(e.work())
	e.exec(`UPDATE tasks SET caps = '{"fix_rounds":0}' WHERE id = ?`, e.task)
	fix := SubmitParams{
		Key:       fmt.Sprintf("task:%d", e.task),
		ProjectID: e.project,
		TaskID:    e.task,
		Kind:      KindFix,
		Mode:      ModeFresh,
		Prompt:    "fix it",
	}
	if res := e.submit(fix); res.Answer != AnsCapped {
		t.Fatalf("fix submit past the cap = %s, want capped", res.Answer)
	}
	if res := e.submit(e.work()); res.Answer != AnsPending {
		t.Fatalf("submit behind the live job = %s, want pending", res.Answer)
	}

	// lease, a plain heartbeat, the sweep's requeue of a lapsed lease,
	// and the revive that follows it.
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("the submitted job was not leased")
	}
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("heartbeat = %s, want ok", ans)
	}
	e.clock.Advance(e.eng.cfg.LeaseTTL + time.Second)
	if rep := e.mustSweep(); len(rep.Expired) != 1 {
		t.Fatalf("the sweep requeued nothing: %+v", rep)
	}
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("reviving heartbeat = %s, want ok", ans)
	}

	// release, then a fresh lease that completes the job. The releaser
	// itself does not see the job again until the exclusion lapses
	// (§12.3), so the second lease is the other runner's.
	if ans := e.release(e.runner1, g, "transient error"); ans != AnsOK {
		t.Fatalf("release = %s, want ok", ans)
	}
	g2 := e.leaseAny(e.runner2)
	if g2 == nil {
		t.Fatal("the released job was not leased again")
	}
	if ans := e.complete(g2, done()); ans != AnsOK {
		t.Fatalf("complete = %s, want ok", ans)
	}

	want := []string{
		evJobCreated, evJobCapped, evJobPending,
		evJobLeased, evTaskRunning, evLeaseRequeued, evLeaseRevived,
		evJobReleased, evJobLeased, evJobCompleted, evTaskReviewing, evJobCreated,
	}
	if got := e.eventKinds(); !equalStrings(got, want) {
		t.Errorf("the job's events:\n got %v\nwant %v", got, want)
	}

	// The payloads say what happened without saying what they must not:
	// the lease appears as its prefix — never the whole capability — the
	// completion as the worker's own status word, the created event with
	// the job's run number.
	rows := e.eventRows()
	if got := rows[0].job; got != r.JobID {
		t.Errorf("created event names job %d, want %d", got, r.JobID)
	}
	if rows[0].payload.Run != 1 {
		t.Errorf("created event run = %d, want 1", rows[0].payload.Run)
	}
	if rows[3].payload.Lease != leasePrefix(g.LeaseID) {
		t.Errorf("leased event lease = %q, want the prefix %q", rows[3].payload.Lease, leasePrefix(g.LeaseID))
	}
	if rows[3].payload.Lease == g.LeaseID {
		t.Error("the leased event carries the whole lease id, the capability itself")
	}
	if got := rows[9].payload.Status; got != statusDone {
		t.Errorf("completed event status = %q, want %q", got, statusDone)
	}
	// The completion moved the task to reviewing and queued the review
	// job (§7): the reviewing event names the state, the review's
	// created event is the key's second run.
	if rows[10].payload.Status != taskReviewing {
		t.Errorf("reviewing event payload = %+v, want the state", rows[10].payload)
	}
	if rows[11].payload.Run != 2 {
		t.Errorf("the review job's created event run = %d, want 2", rows[11].payload.Run)
	}
}

func TestEventsTraceTheFailuresAndTheFence(t *testing.T) {
	t.Run("queue TTL", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) { c.QueueTTL = time.Minute })
		e.submit(e.work())
		e.clock.Advance(2 * time.Minute)
		if rep := e.mustSweep(); len(rep.QueueTimedOut) != 1 {
			t.Fatalf("the sweep timed nothing out: %+v", rep)
		}
		if got := e.eventKinds(); !equalStrings(got, []string{evJobCreated, evJobQueueTimedOut}) {
			t.Errorf("events = %v", got)
		}
		last := e.eventRows()[1]
		if last.payload.Status != statusFailed || last.payload.Reason == "" {
			t.Errorf("queue-TTL event payload = %+v, want the failure status and the reason", last.payload)
		}
	})

	t.Run("hard cap", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) {
			c.HardCaps = map[Kind]time.Duration{KindWork: time.Minute}
			c.LeaseTTL = 10 * time.Minute // the cap fires, not the expiry
		})
		e.submit(e.work())
		e.leaseAny(e.runner2)
		e.clock.Advance(2 * time.Minute)
		if rep := e.mustSweep(); len(rep.HardCapped) != 1 {
			t.Fatalf("the sweep hard-capped nothing: %+v", rep)
		}
		if got := e.eventKinds(); !equalStrings(got, []string{evJobCreated, evJobLeased, evTaskRunning, evJobHardCapped}) {
			t.Errorf("events = %v", got)
		}
		if last := e.eventRows()[3]; last.payload.Status != statusFailed || last.payload.Reason == "" {
			t.Errorf("hard-cap event payload = %+v, want the failure status and the reason", last.payload)
		}
	})

	t.Run("attempts exceeded", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) { c.MaxAttempts = 1 })
		e.submit(e.work())
		e.leaseAny(e.runner1)
		// A release would refund the attempt; a lost lease — the expiry
		// — is what spends it (§12.3).
		e.clock.Advance(e.eng.cfg.LeaseTTL + time.Second)
		if rep := e.mustSweep(); len(rep.Expired) != 1 {
			t.Fatalf("the sweep requeued nothing: %+v", rep)
		}
		if got := e.leaseAny(e.runner2); got != nil {
			e.t.Fatal("the job was offered past its attempt cap")
		}
		want := []string{evJobCreated, evJobLeased, evTaskRunning, evLeaseRequeued, evJobAttemptsExceeded}
		if got := e.eventKinds(); !equalStrings(got, want) {
			t.Errorf("events = %v, want %v", got, want)
		}
		if last := e.eventRows()[4]; last.payload.Status != statusFailed || last.payload.Reason == "" {
			t.Errorf("attempt-cap event payload = %+v, want the failure status and the reason", last.payload)
		}
	})

	t.Run("cancel and fence", func(t *testing.T) {
		e := newEnv(t)
		r := e.submit(e.work())
		if ans := e.cancel(r.JobID); ans != AnsOK {
			t.Fatalf("cancel = %s, want ok", ans)
		}
		if _, err := e.eng.BumpFenceEpoch(e.ctx); err != nil {
			t.Fatalf("bump the fence epoch: %v", err)
		}
		want := []string{evJobCreated, evJobCancelled, evFenceEpochBumped}
		if got := e.eventKinds(); !equalStrings(got, want) {
			t.Errorf("events = %v, want %v", got, want)
		}
		rows := e.eventRows()
		if rows[1].payload.Reason != "" {
			t.Errorf("a plain cancel's reason = %q, want none", rows[1].payload.Reason)
		}
		if rows[2].payload.Epoch != 2 {
			t.Errorf("fence event epoch = %d, want 2", rows[2].payload.Epoch)
		}
	})
}

func TestStopOutcomesNameThemselvesInTheEvent(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		result  string
	}{
		{OutcomeStalled, `{"status":"stalled","reason":"no activity for over 10m"}`},
		{OutcomeTimeCap, `{"status":"time_cap","reason":"the run passed its class time cap"}`},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			e := newEnv(t)
			e.submit(e.work())
			g := e.leaseAny(e.runner1)
			e.complete(g, Completion{Result: tc.result, Outcome: tc.outcome})
			rows := e.eventRows()
			// The park wakes the foreman (§4.2): the turn's job and its
			// event follow the task's.
			if len(rows) != 7 || rows[3].kind != evJobCompleted {
				t.Fatalf("events = %v", e.eventKinds())
			}
			if rows[3].payload.Status != tc.outcome {
				t.Errorf("completion event status = %q, want %q", rows[3].payload.Status, tc.outcome)
			}
			// A stopped run always goes to the foreman (§5.6): the task
			// parks at needs_foreman with the failure's words.
			if rows[4].kind != evTaskFailed || rows[4].payload.Status != taskNeedsForeman {
				t.Errorf("the stopped run's task event = %s %+v, want %s with the state", rows[4].kind, rows[4].payload, evTaskFailed)
			}
			if rows[6].kind != evForemanWoken || rows[6].payload.Count != 1 {
				t.Errorf("the park's wake event = %s %+v, want %s counting one event", rows[6].kind, rows[6].payload, evForemanWoken)
			}
		})
	}
}

// errBoom is the failure the same-transaction test injects after a path
// has done its work, standing in for any later statement failing.
var errBoom = errors.New("boom: the rest of the transaction failed")

// TestEventsShareTheChangeTransaction is the property the log rests on
// (§9): every path writes its event inside the transaction that makes
// the change, so a transaction that fails writes none of it. Each case
// runs the path's own tx function and then fails the surrounding
// transaction on purpose; nothing — not the change, not its event, not
// the message — may survive.
func TestEventsShareTheChangeTransaction(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env) *LeaseGrant
		run   func(e *env, tx *sql.Tx, g *LeaseGrant) error
	}{
		{
			name:  "submit",
			setup: func(e *env) *LeaseGrant { return nil },
			run: func(e *env, tx *sql.Tx, _ *LeaseGrant) error {
				_, err := e.eng.submitTx(tx, e.work(), e.clock.Now())
				return err
			},
		},
		{
			name: "lease",
			setup: func(e *env) *LeaseGrant {
				e.submit(e.work())
				return nil
			},
			run: func(e *env, tx *sql.Tx, _ *LeaseGrant) error {
				_, err := e.eng.leaseTx(tx, e.runner1, nil, nil, e.clock.Now())
				return err
			},
		},
		{
			name: "heartbeat revive",
			setup: func(e *env) *LeaseGrant {
				e.submit(e.work())
				g := e.leaseAny(e.runner1)
				e.clock.Advance(e.eng.cfg.LeaseTTL + time.Second)
				e.mustSweep()
				return g
			},
			run: func(e *env, tx *sql.Tx, g *LeaseGrant) error {
				_, _, err := e.eng.heartbeatTx(tx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{}, SweepReport{}, e.clock.Now())
				return err
			},
		},
		{
			name: "complete",
			setup: func(e *env) *LeaseGrant {
				e.submit(e.work())
				return e.leaseAny(e.runner1)
			},
			run: func(e *env, tx *sql.Tx, g *LeaseGrant) error {
				_, err := e.eng.completeTx(tx, g.LeaseOwner, g.JobID, g.LeaseID, done(), e.clock.Now())
				return err
			},
		},
		{
			name: "release",
			setup: func(e *env) *LeaseGrant {
				e.submit(e.work())
				return e.leaseAny(e.runner1)
			},
			run: func(e *env, tx *sql.Tx, g *LeaseGrant) error {
				_, err := e.eng.releaseTx(tx, g.LeaseOwner, g.JobID, g.LeaseID, "transient", e.clock.Now())
				return err
			},
		},
		{
			name: "cancel",
			setup: func(e *env) *LeaseGrant {
				r := e.submit(e.work())
				// A stand-in grant carrying the job id; cancelTx reads
				// the row itself inside the transaction.
				return &LeaseGrant{JobID: r.JobID}
			},
			run: func(e *env, tx *sql.Tx, g *LeaseGrant) error {
				job, err := loadJobTx(tx, g.JobID)
				if err != nil {
					return err
				}
				_, err = e.eng.cancelTx(tx, job, "", e.clock.Now())
				return err
			},
		},
		{
			name: "sweep",
			setup: func(e *env) *LeaseGrant {
				e.submit(e.work())
				g := e.leaseAny(e.runner1)
				e.clock.Advance(e.eng.cfg.LeaseTTL + time.Second)
				return g
			},
			run: func(e *env, tx *sql.Tx, _ *LeaseGrant) error {
				_, err := e.eng.sweepTx(tx, e.clock.Now())
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			g := tc.setup(e)
			// What setup wrote legitimately stays; the failed
			// transaction must leave exactly these rows behind.
			before := map[string]int64{}
			for _, table := range []string{"events", "messages", "jobs"} {
				before[table] = e.int("SELECT COUNT(*) FROM " + table)
			}
			err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
				if err := tc.run(e, tx, g); err != nil {
					return err
				}
				return errBoom
			})
			if !errors.Is(err, errBoom) {
				t.Fatalf("the injected failure did not surface: %v", err)
			}
			for _, table := range []string{"events", "messages", "jobs"} {
				if got := e.int("SELECT COUNT(*) FROM " + table); got != before[table] {
					t.Errorf("%s survived the rollback: %d rows, want the %d setup wrote", table, got, before[table])
				}
			}
		})
	}
}

// TestWorkerResultMessageShapes pins how the history renders each kind
// of completion result: a worker's result block by its fields, a
// reviewer's verdict block by its verdict and findings — the shape a
// review job completes with — and anything else verbatim under the
// unknown status.
func TestWorkerResultMessageShapes(t *testing.T) {
	cases := []struct {
		name, result, want string
	}{
		{
			name:   "worker result block",
			result: `{"status":"done","summary":"edited work.txt","branch":"brigada/task-1"}`,
			want:   `{"status":"done","summary":"edited work.txt","branch":"brigada/task-1"}`,
		},
		{
			name:   "review approve",
			result: `{"verdict": "approve", "findings": "checked the diff against the spec"}`,
			want:   `{"verdict":"approve","findings":"checked the diff against the spec"}`,
		},
		{
			name:   "review changes",
			result: `{"verdict":"changes","findings":"the test misses the error path"}`,
			want:   `{"verdict":"changes","findings":"the test misses the error path"}`,
		},
		{
			name:   "no block at all",
			result: `I finished everything, trust me`,
			want:   `{"status":"unknown","summary":"I finished everything, trust me"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workerResultMessage(tc.result); got != tc.want {
				t.Errorf("workerResultMessage(%s) = %s, want %s", tc.result, got, tc.want)
			}
		})
	}
}
