package engine

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func usageInt(n int64) *int64       { return &n }
func usageFloat(n float64) *float64 { return &n }

func TestCompletionUsageAndOlderRunner(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "older", true: "reported"}[known], func(t *testing.T) {
			e := newEnv(t)
			e.addAgent(e.runner1, "claude", "sonnet", "work")
			job := e.submit(e.work())
			g := e.leaseAny(e.runner1)
			if g == nil {
				t.Fatal("no lease")
			}
			comp := done()
			if known {
				comp.Usage = &adapter.Usage{InputTokens: usageInt(0), OutputTokens: usageInt(7), CostUSD: usageFloat(0)}
				comp.WallSeconds = usageFloat(2.5)
			}
			if ans := e.complete(g, comp); ans != AnsOK {
				t.Fatal(ans)
			}
			if ans := e.complete(g, comp); ans != AnsAlready {
				t.Fatal(ans)
			}
			err := e.st.View(e.ctx, func(tx *sql.Tx) error {
				var input, output, cached, reasoning sql.NullInt64
				var cost, wall sql.NullFloat64
				if err := tx.QueryRow(`SELECT input_tokens,output_tokens,cached_input_tokens,reasoning_tokens,cost_usd,wall_seconds FROM jobs WHERE id=?`, job.JobID).Scan(&input, &output, &cached, &reasoning, &cost, &wall); err != nil {
					return err
				}
				if known {
					if !input.Valid || input.Int64 != 0 || !output.Valid || output.Int64 != 7 || !cost.Valid || cost.Float64 != 0 || !wall.Valid || wall.Float64 != 2.5 {
						t.Fatalf("reported usage lost: %v %v %v %v", input, output, cost, wall)
					}
				} else if input.Valid || output.Valid || cost.Valid || wall.Valid {
					t.Fatal("older runner usage is known")
				}
				if cached.Valid || reasoning.Valid {
					t.Fatal("missing breakdown became known")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStatsUsageAllJobKinds(t *testing.T) {
	e := newEnv(t)
	author := e.addAgent(e.runner1, "claude", "sonnet", "work")
	reviewer := e.addAgent(e.runner2, "codex", "gpt", "review")
	// Different classes and agents must not leak into each other's sums.
	for _, entry := range []struct {
		kind          string
		agent         int64
		input, output int
		cost          any
	}{
		{"work", author, 10, 5, 0.25}, {"fix", author, 20, 7, nil}, {"review", reviewer, 30, 9, nil},
		{"work", reviewer, 1, 1, nil},
	} {
		e.exec(`INSERT INTO jobs(project_id,task_id,kind,key,agent_id,mode,prompt,status,input_tokens,output_tokens,cached_input_tokens,reasoning_tokens,cost_usd,wall_seconds,created_at,updated_at)
   VALUES(?,?,?,?,?,'fresh','prompt','done',?,?,0,0,?,2.5,?,?)`, e.project, e.task, entry.kind, "usage:"+entry.kind+fmt.Sprint(entry.agent), entry.agent, entry.input, entry.output, entry.cost, seedTS, seedTS)
	}
	// An old job must not turn unknown counters into zero or poison sums.
	e.exec(`INSERT INTO jobs(project_id,task_id,kind,key,agent_id,mode,prompt,status,created_at,updated_at) VALUES(?,?,'work','usage:old',?,'fresh','prompt','failed',?,?)`, e.project, e.task, author, seedTS, seedTS)
	for _, entry := range []struct {
		agent int64
		class string
	}{{author, "implement"}, {reviewer, "review"}} {
		e.exec(`INSERT INTO outcomes(project_id,agent_id,class,attempts,successes,updated_at) VALUES(?,?,?,3,2,?)`, e.project, entry.agent, entry.class, seedTS)
	}
	stats, err := e.eng.Stats(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range stats[0].Rows {
		switch {
		case row.AgentID == author && row.Class == "implement":
			if row.InputTokens == nil || *row.InputTokens != 30 || row.Tokens == nil || *row.Tokens != 42 || row.CostUSD == nil || *row.CostUSD != 0.25 || row.WallSeconds == nil || *row.WallSeconds != 5 || row.CostPerSuccess == nil || *row.CostPerSuccess != 0.125 || row.CostPerSuccessUnit != "USD" {
				t.Fatalf("author: %+v", row)
			}
		case row.AgentID == reviewer && row.Class == "review":
			if row.Tokens == nil || *row.Tokens != 39 || row.CostUSD != nil || row.CostPerSuccess == nil || *row.CostPerSuccess != 19.5 || row.CostPerSuccessUnit != "tokens" {
				t.Fatalf("reviewer: %+v", row)
			}
		case row.AgentID == reviewer && row.Class == "implement":
			if row.Tokens == nil || *row.Tokens != 2 || row.CostPerSuccess != nil {
				t.Fatalf("reviewer work: %+v", row)
			}
		default:
			if row.Tokens != nil || row.CostPerSuccess != nil {
				t.Fatalf("cold start has usage: %+v", row)
			}
		}
	}
}

// Two jobs on one Claude session report the session's running total; each
// job records its own delta, and the sums (and the plan budget) add deltas.
func TestResumedSessionCostIsPerJob(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.setSettings(`{"approval_budget": "$2"}`)
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer"},
		{Name: "c", Spec: "c", Class: "answer", Deps: []string{"a", "b"}},
	})
	for _, total := range []float64{1.0, 1.75} {
		g := e.leaseAny(e.runner1)
		if g == nil {
			t.Fatal("no lease")
		}
		c := done()
		c.SessionID = "sess-resumed"
		c.Usage = &adapter.Usage{CostUSD: usageFloat(total), CostCumulative: true}
		if ans := e.complete(g, c); ans != AnsOK {
			t.Fatal(ans)
		}
	}
	var costs []float64
	var sessions []float64
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT cost_usd, session_cost_usd FROM jobs WHERE cost_usd IS NOT NULL ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c, s float64
			if err := rows.Scan(&c, &s); err != nil {
				return err
			}
			costs = append(costs, c)
			sessions = append(sessions, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 2 || costs[0] != 1.0 || costs[1] != 0.75 || sessions[1] != 1.75 {
		t.Fatalf("costs = %v sessions = %v, want deltas 1 and 0.75 of total 1.75", costs, sessions)
	}
	if got := e.taskState(res.Tasks[2].TaskID); got != taskReady {
		t.Fatalf("c = %q, want ready: summed deltas $1.75 are under the $2 budget", got)
	}
	stats, err := e.eng.Stats(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, row := range stats[0].Rows {
		if row.CostUSD != nil {
			total += *row.CostUSD
		}
	}
	if total != 1.75 {
		t.Fatalf("stats cost = %v, want 1.75", total)
	}
}
