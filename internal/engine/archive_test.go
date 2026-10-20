package engine

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

const archiveRepo = "https://example.invalid/brigada.git"

func TestArchiveProjectRefusesLiveWorkAndCancelsAtomically(t *testing.T) {
	for _, status := range []string{"queued", "leased", "running"} {
		t.Run(status, func(t *testing.T) {
			e := newEnv(t)
			e.addAgent(e.runner1, "claude", "sonnet", "work")
			job := e.submit(e.work())
			if status != "queued" {
				if _, err := e.eng.Lease(e.ctx, e.runner1, []string{"claude"}, nil); err != nil {
					t.Fatal(err)
				}
				if status == "running" {
					e.exec("UPDATE jobs SET status = 'running' WHERE id = ?", job.JobID)
				}
			}
			if _, err := e.eng.ArchiveProject(e.ctx, archiveRepo, false); !errors.Is(err, ErrProjectBusy) || !strings.Contains(err.Error(), "use cancel") {
				t.Fatalf("remove live work: %v", err)
			}
			if e.str("SELECT archived_at FROM projects WHERE id = ?", e.project) != "" || e.str("SELECT status FROM jobs WHERE id = ?", job.JobID) != status {
				t.Fatal("refused removal changed project or job")
			}
			answer, err := e.eng.ArchiveProject(e.ctx, archiveRepo, true)
			if err != nil || answer != AnsOK {
				t.Fatalf("remove with cancel: %s %v", answer, err)
			}
			if e.str("SELECT status FROM jobs WHERE id = ?", job.JobID) != "cancelled" || e.str("SELECT state FROM plans WHERE id = ?", e.plan) != "cancelled" || e.str("SELECT state FROM tasks WHERE id = ?", e.task) != "cancelled" {
				t.Fatal("removal left work live")
			}
			if e.int("SELECT COUNT(*) FROM events WHERE kind = 'job_cancelled' AND job_id = ?", job.JobID) != 1 {
				t.Fatal("job did not use cancellation path exactly once")
			}
			stamp := e.str("SELECT archived_at FROM projects WHERE id = ?", e.project)
			e.clock.Advance(time.Second)
			if answer, err := e.eng.ArchiveProject(e.ctx, archiveRepo, false); err != nil || answer != AnsAlready || stamp != e.str("SELECT archived_at FROM projects WHERE id = ?", e.project) {
				t.Fatalf("repeat removal: %s %v", answer, err)
			}
			if grant, err := e.eng.Lease(e.ctx, e.runner1, []string{"claude"}, nil); err != nil || grant != nil {
				t.Fatalf("removed work was leased: %+v %v", grant, err)
			}
		})
	}
}

func TestArchiveProjectRefusesUnfinishedPlansWithoutJobs(t *testing.T) {
	for _, state := range []string{"draft", "pending_approval", "active"} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t)
			e.exec("UPDATE plans SET state = ? WHERE id = ?", state, e.plan)
			if _, err := e.eng.ArchiveProject(e.ctx, archiveRepo, false); !errors.Is(err, ErrProjectBusy) {
				t.Fatalf("remove %s plan: %v", state, err)
			}
			if _, err := e.eng.ArchiveProject(e.ctx, archiveRepo, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArchivedProjectWorkGates(t *testing.T) {
	e := newEnv(t)
	if _, err := e.eng.ArchiveProject(e.ctx, archiveRepo, true); err != nil {
		t.Fatal(err)
	}
	calls := map[string]func() error{
		"explicit plan recovery": func() error { _, _, err := e.eng.RecoverPlans(e.ctx, []string{archiveRepo}); return err },
		"foreman turn":           func() error { _, err := e.eng.SubmitForemanTurn(e.ctx, e.project, nil); return err },
		"review":                 func() error { _, err := e.eng.ReviewBranch(e.ctx, archiveRepo, "feature"); return err },
		"plan": func() error {
			_, err := e.eng.CreatePlan(e.ctx, e.project, "new plan", []PlanTask{{Spec: "work"}})
			return err
		},
		"amend": func() error {
			_, err := e.eng.AmendPlan(e.ctx, e.project, e.plan, PlanAmendment{Add: []PlanTask{{Spec: "work"}}})
			return err
		},
		"approve": func() error { _, err := e.eng.ApprovePlan(e.ctx, e.project, e.plan); return err },
		"submit":  func() error { _, err := e.eng.Submit(e.ctx, e.work()); return err },
		"submit task": func() error {
			_, err := e.eng.SubmitTask(e.ctx, TaskParams{RepoURL: archiveRepo, BaseRef: "main", Spec: "work"})
			return err
		},
		"attach":       func() error { _, err := e.eng.Attach(e.ctx, e.project, true); return err },
		"ask":          func() error { _, err := e.eng.AskForeman(e.ctx, archiveRepo, "help", "user"); return err },
		"implicit ask": func() error { _, err := e.eng.AskForeman(e.ctx, "", "help", "user"); return err },
		"ask user":     func() error { _, err := e.eng.AskUser(e.ctx, e.project, "help?"); return err },
		"answer":       func() error { _, err := e.eng.Answer(e.ctx, e.project, e.task, "yes"); return err },
		"restart":      func() error { _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "retry"); return err },
		"events":       func() error { _, err := e.eng.ForemanEvents(e.ctx, e.project); return err },
		"wake budget":  func() error { _, err := e.eng.SetWakeBudget(e.ctx, archiveRepo, 10); return err },
		"wake": func() error {
			return e.st.Update(e.ctx, func(tx *sql.Tx) error { _, err := e.eng.wakeOutcomeTx(tx, e.project, e.clock.Now()); return err })
		},
		// Inject a stale queued row to exercise the defensive lease boundary.
		"lease": func() error {
			e.exec(`INSERT INTO jobs (project_id,task_id,kind,key,mode,prompt,status,created_at,updated_at) VALUES (?,?,'work','stale','fresh','work','queued',?,?)`, e.project, e.task, seedTS, seedTS)
			_, err := e.eng.Lease(e.ctx, e.runner1, []string{"claude"}, nil)
			e.exec("DELETE FROM jobs WHERE key = 'stale'")
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			before := e.int("SELECT COUNT(*) FROM events")
			err := call()
			if !errors.Is(err, ErrProjectArchived) || !strings.Contains(err.Error(), "was removed; run brigada init in it to bring it back") {
				t.Fatalf("archived project refusal: %v", err)
			}
			if after := e.int("SELECT COUNT(*) FROM events"); before != after {
				t.Fatal("refused call wrote events")
			}
		})
	}
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("archived inbox broke background recovery: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE status IN ('queued','leased','running')"); n != 0 {
		t.Fatalf("background recovery resurrected %d jobs", n)
	}
}

func TestArchiveReleasesAttachHold(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE plans SET state = 'done' WHERE id = ?", e.plan)
	h, err := e.eng.Attach(e.ctx, e.project, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.ArchiveProject(e.ctx, archiveRepo, false); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err := e.eng.HoldTokenScope(e.ctx, h.ForemanToken)
	if err != nil || ok {
		t.Fatalf("attach token survived: %v %v", ok, err)
	}
}

func TestArchivedProjectVisibilityAndRegistration(t *testing.T) {
	for _, via := range []string{"foreman", "import"} {
		t.Run(via, func(t *testing.T) {
			e := newEnv(t)
			if _, err := e.eng.CanonicalizeProjects(e.ctx); err != nil {
				t.Fatal(err)
			}
			agent := e.addAgent(e.runner1, "claude", "sonnet", "work")
			e.submit(e.work())
			e.exec(`INSERT INTO outcomes (project_id,agent_id,class,attempts,successes,updated_at) VALUES (?,?,'implement',4,3,?)`, e.project, agent, seedTS)
			if _, err := e.eng.ArchiveProject(e.ctx, archiveRepo, true); err != nil {
				t.Fatal(err)
			}
			projects, err := e.eng.Projects(e.ctx)
			if err != nil || len(projects) != 0 {
				t.Fatalf("project list: %+v %v", projects, err)
			}
			for _, scope := range []int64{0, e.project} {
				status, err := e.eng.StatusWithInstructions(e.ctx, scope)
				if err != nil {
					t.Fatal(err)
				}
				if len(status.Slots)+len(status.Plans)+len(status.NeedsYou)+len(status.Instructions)+len(status.Running)+len(status.Held) != 0 || status.ForemanMessages != 0 || status.Report != nil || status.Jobs["cancelled"] != 0 {
					t.Fatalf("archived status leaked: %+v", status)
				}
				stats, err := e.eng.Stats(e.ctx, scope)
				if err != nil || len(stats) != 1 {
					t.Fatalf("stats lost archived project: %+v %v", stats, err)
				}
				found := false
				for _, row := range stats[0].Rows {
					if row.AgentID == agent && row.Class == "implement" && row.Attempts == 4 && row.Successes == 3 {
						found = true
					}
				}
				if !found {
					t.Fatal("stats lost past outcomes")
				}
			}
			history, err := e.eng.History(e.ctx, HistoryQuery{})
			if err != nil || len(history.Items) == 0 {
				t.Fatalf("history lost: %+v %v", history, err)
			}
			jobs := e.int("SELECT COUNT(*) FROM jobs WHERE project_id = ?", e.project)
			if via == "foreman" {
				setting, err := e.eng.SetProjectForeman(e.ctx, "git@example.invalid:brigada.git", "claude", "sonnet")
				if err != nil || setting.ProjectID != e.project {
					t.Fatalf("restore foreman: %+v %v", setting, err)
				}
			} else {
				res, err := e.eng.ImportProjectConfig(e.ctx, archiveRepo, cfgFile(""), false)
				if err != nil || res.Settings == nil || res.Settings.ProjectID != e.project {
					t.Fatalf("restore import: %+v %v", res, err)
				}
			}
			if e.int("SELECT COUNT(*) FROM projects") != 1 || e.str("SELECT archived_at FROM projects WHERE id = ?", e.project) != "" || jobs != e.int("SELECT COUNT(*) FROM jobs WHERE project_id = ?", e.project) {
				t.Fatal("registration did not restore the original row and jobs")
			}
			if _, err := e.eng.CreatePlan(e.ctx, e.project, "restored work", []PlanTask{{Spec: "work"}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArchiveCancelsForemanLeaseAndKeepsInbox(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	asked, err := e.eng.AskForeman(e.ctx, archiveRepo, "retain my request", "user")
	if err != nil {
		t.Fatal(err)
	}
	grant, err := e.eng.Lease(e.ctx, e.runner1, []string{"claude"}, nil)
	if err != nil || grant == nil || grant.ForemanToken == "" {
		t.Fatalf("lease foreman: %+v %v", grant, err)
	}
	if _, err := e.eng.ArchiveProject(e.ctx, archiveRepo, true); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := e.eng.LeaseTokenScope(e.ctx, grant.ForemanToken); err != nil || ok {
		t.Fatalf("cancelled foreman's token survived: %v %v", ok, err)
	}
	if got := e.str("SELECT state FROM foreman_inbox WHERE event_id = ?", asked.EventID); got != inboxPending {
		t.Fatalf("user request lost after cancellation: %q", got)
	}
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.int("SELECT COUNT(*) FROM jobs WHERE status IN ('queued','leased','running')") != 0 {
		t.Fatal("removed project woke again")
	}
}

func TestArchiveHidesCompletedPlansReportsAndPendingMessages(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE plans SET state = 'done' WHERE id = ?", e.plan)
	e.exec("UPDATE tasks SET state = 'needs_foreman' WHERE id = ?", e.task)
	e.exec(`UPDATE projects SET settings = '{"instructions":"private project guidance"}', foreman_limited = 'unknown' WHERE id = ?`, e.project)
	if _, err := e.eng.Attach(e.ctx, e.project, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.AskForeman(e.ctx, archiveRepo, "pending message", "user"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := appendEventTx(tx, evForemanReport, e.project, 0, 0, eventPayload{Report: "old report"}, e.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, err := e.eng.StatusWithInstructions(e.ctx, e.project)
	if err != nil || len(before.Plans) == 0 || len(before.Instructions) == 0 || len(before.NeedsYou) == 0 || before.ForemanMessages == 0 || before.Report == nil {
		t.Fatalf("fixture missing visible state: %+v %v", before, err)
	}
	if _, err := e.eng.ArchiveProject(e.ctx, archiveRepo, false); err != nil {
		t.Fatal(err)
	}
	for _, project := range []int64{0, e.project} {
		after, err := e.eng.StatusWithInstructions(e.ctx, project)
		if err != nil || len(after.Plans)+len(after.Instructions)+len(after.NeedsYou)+len(after.Slots) != 0 || after.ForemanMessages != 0 || after.Report != nil {
			t.Fatalf("status still shows removed project: %+v %v", after, err)
		}
	}
	page, err := e.eng.History(e.ctx, HistoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 || e.int("SELECT COUNT(*) FROM messages WHERE project_id = ?", e.project) == 0 {
		t.Fatal("history was removed with project")
	}
}
