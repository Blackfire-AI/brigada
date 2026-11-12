package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/approval"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// The authority behind the gates (§12.2). A gate setting (gate_plan,
// gate_land, gate_push, approval_tiers) still decides what waits for the
// user; once something waits, the act that releases or cancels it — the
// user's yes or no on a plan at its version, on a landing, on a push —
// runs only when a signature from an enrolled approval key verifies for
// the one-time challenge of exactly that act. A token says who may call
// and carries no approval authority, the admin entry of <state>/tokens
// included.
//
// GatedAct is the one call an API handler makes for those acts. Asked
// with no request, it makes a pending request (the store's
// approval_requests table), says so in the needs-you queue and the log,
// and answers the challenge to sign out of band; it does not wait on a
// terminal and prompts on none. Asked with the request's id and a
// signature, it verifies and runs the act once. The plain engine calls
// (ApprovePlanVersion, LandApprove, PushApprove and their rejecting
// twins) carry no signature check: they are what a verified GatedAct
// calls, and what the engine's own tests drive.

// GatedAction names one of the acts GatedAct covers.
type GatedAction string

const (
	GatedPlanApprove GatedAction = "plan_approve" // ApprovePlanVersion
	GatedPlanReject  GatedAction = "plan_reject"  // RejectPlanVersion
	GatedLandApprove GatedAction = "land_approve" // LandApprove
	GatedLandReject  GatedAction = "land_reject"  // LandReject
	GatedPushApprove GatedAction = "push_approve" // PushApprove
)

// GatedActRequest is the act a caller asks for, in the terms of the
// engine call behind it.
type GatedActRequest struct {
	Action GatedAction
	// Project scopes a plan or landing act to one project, as the plain
	// calls' project argument does; 0 names any. It is unused by a push.
	Project int64
	// Plan and Version name a plan act. Version 0 binds the plan's
	// current version into the challenge, so the user signs the version
	// the engine shows; an amendment since then refuses the act as stale.
	Plan    int64
	Version int64
	// Task names a landing act.
	Task int64
	// StopAsking is the approve calls' option of the same name; it is part
	// of the act signed, so a signature without it cannot change a rule.
	StopAsking bool
	// RepoURL names the project of a push act.
	RepoURL string
}

// PendingApproval is the challenge a gated act waits on.
type PendingApproval struct {
	RequestID int64
	Act       string // the act in words, such as "approve plan #7 at version 2"
	Challenge string // the one-time nonce, the challenge's name
	// Message is the exact text to sign with an enrolled key under
	// Namespace: ssh-keygen -Y sign -n <Namespace> over it.
	Message   string
	Namespace string
	Expires   time.Time
	// NoKey is set when no approval key is enrolled yet: the request
	// waits all the same, and no signature can verify until one is.
	NoKey bool
	// Wanted is the one line every surface prints for this request.
	Wanted string
}

// GatedResult is what GatedAct did. Pending is set when the act waits
// for a signature; otherwise exactly one of Plan, Land and Push holds the
// result of the act that ran.
type GatedResult struct {
	Pending *PendingApproval
	Plan    *PlanDecision
	Land    *LandDecision
	Push    *PushApproveResult
}

// ErrSignatureWithoutRequest refuses a signature that names no request.
var ErrSignatureWithoutRequest = fmt.Errorf("%w: a signature answers a pending request; ask for the act first and sign its challenge", approval.ErrNoRequest)

// gatedAct is a request bound to the engine's rows: the project and the
// version resolved, and the act's identity as the challenge names it.
type gatedAct struct {
	req      GatedActRequest
	project  int64
	version  int64
	kind     string
	identity string
}

func (e *Engine) approvalAuthority() *approval.Authority {
	e.approverOnce.Do(func() {
		a := approval.NewWithClock(approval.NewStore(e.cfg.ApprovalKeys), e.cfg.Now)
		// What the last run already decided stays spent.
		_ = a.LoadSpent(context.Background(), e.store.Store)
		e.approver = a
	})
	return e.approver
}

// GatedAct is the call behind the approval of a gated act (§12.2).
//
// With requestID 0 and no signature it asks: it returns the pending
// request of the act, a new one or the one already waiting for the same
// act, with its challenge, and runs nothing. With a requestID and the
// signature of that request's challenge it answers: the signature must
// verify against an enrolled approval key for exactly this act, and then
// the act runs once. Every refusal wraps one reason of internal/approval
// (ErrNoKey, ErrNoSignature, ErrBadSignature, ErrReused, ErrExpired,
// ErrWrongAct, ErrRefused, ErrNoRequest) and leaves a waiting request
// waiting, to be signed again; a spent or expired one is asked for anew.
//
// The caller has checked the token's role; this checks the signature.
func (e *Engine) GatedAct(ctx context.Context, act GatedActRequest, requestID int64, signature []byte) (GatedResult, error) {
	bound, err := e.bindGated(ctx, act)
	if err != nil {
		return GatedResult{}, err
	}
	if requestID == 0 {
		if len(signature) > 0 {
			return GatedResult{}, ErrSignatureWithoutRequest
		}
		p, err := e.askGated(ctx, bound)
		if err != nil {
			return GatedResult{}, err
		}
		return GatedResult{Pending: p}, nil
	}
	if _, err := e.approvalAuthority().Approve(ctx, e.store.Store, requestID,
		approval.Act{Kind: bound.kind, ID: bound.identity}, signature); err != nil {
		return GatedResult{}, err
	}
	// The request is spent: the act runs once, and a failure of the act
	// itself (a plan amended meanwhile, a hold already released) asks for
	// a fresh challenge rather than reusing this one.
	var res GatedResult
	switch act.Action {
	case GatedPlanApprove:
		d, err := e.ApprovePlanVersion(ctx, act.Project, act.Plan, bound.version, act.StopAsking)
		if err != nil {
			return GatedResult{}, err
		}
		res.Plan = &d
	case GatedPlanReject:
		d, err := e.RejectPlanVersion(ctx, act.Project, act.Plan, bound.version)
		if err != nil {
			return GatedResult{}, err
		}
		res.Plan = &d
	case GatedLandApprove:
		d, err := e.LandApprove(ctx, bound.project, act.Task, act.StopAsking)
		if err != nil {
			return GatedResult{}, err
		}
		res.Land = &d
	case GatedLandReject:
		d, err := e.LandReject(ctx, bound.project, act.Task)
		if err != nil {
			return GatedResult{}, err
		}
		res.Land = &d
	case GatedPushApprove:
		d, err := e.PushApprove(ctx, act.RepoURL)
		if err != nil {
			return GatedResult{}, err
		}
		res.Push = &d
	}
	return res, nil
}

// bindGated resolves the rows a request names and builds the act's
// identity, which carries the project, the plan version and the options,
// so a signature covers exactly that and nothing else.
func (e *Engine) bindGated(ctx context.Context, act GatedActRequest) (gatedAct, error) {
	b := gatedAct{req: act}
	stop := 0
	if act.StopAsking && (act.Action == GatedPlanApprove || act.Action == GatedLandApprove) {
		stop = 1
	}
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		switch act.Action {
		case GatedPlanApprove, GatedPlanReject:
			var have int64
			err := tx.QueryRow("SELECT project_id, version FROM plans WHERE id = ?", act.Plan).Scan(&b.project, &have)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && act.Project != 0 && b.project != act.Project) {
				return fmt.Errorf("%w: %d", ErrNoPlan, act.Plan)
			}
			if err != nil {
				return fmt.Errorf("read plan %d: %w", act.Plan, err)
			}
			b.version = act.Version
			if b.version == 0 {
				b.version = have
			}
			verb := "approve"
			if act.Action == GatedPlanReject {
				verb = "reject"
			}
			b.kind = approval.KindPlan
			b.identity = fmt.Sprintf("%s:project=%d:plan=%d:v=%d:stop=%d", verb, b.project, act.Plan, b.version, stop)
		case GatedLandApprove, GatedLandReject:
			err := tx.QueryRow(`SELECT p.project_id FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE t.id = ?`, act.Task).Scan(&b.project)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && act.Project != 0 && b.project != act.Project) {
				return fmt.Errorf("no such task %d", act.Task)
			}
			if err != nil {
				return fmt.Errorf("read task %d: %w", act.Task, err)
			}
			verb := "land-approve"
			if act.Action == GatedLandReject {
				verb = "land-reject"
			}
			b.kind = approval.KindTask
			b.identity = fmt.Sprintf("%s:project=%d:task=%d:stop=%d", verb, b.project, act.Task, stop)
		case GatedPushApprove:
			if act.RepoURL == "" {
				return fmt.Errorf("repo URL is required (product §5 rule 4: repos are addressed by URL)")
			}
			err := lookupProjectTx(tx, act.RepoURL, "id", &b.project)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no project for repo %s", RedactRepoURL(act.RepoURL))
			}
			if err != nil {
				return fmt.Errorf("read the project of %s: %w", RedactRepoURL(act.RepoURL), err)
			}
			b.kind = approval.KindPush
			b.identity = fmt.Sprintf("approve:project=%d", b.project)
		default:
			return fmt.Errorf("unknown gated act %q", act.Action)
		}
		return nil
	})
	return b, err
}

// askGated makes, or finds, the pending request of the bound act.
func (e *Engine) askGated(ctx context.Context, b gatedAct) (*PendingApproval, error) {
	now := e.cfg.Now()
	waiting, err := e.store.WaitingApprovalRequests(ctx, now)
	if err != nil {
		return nil, err
	}
	var row store.ApprovalRequest
	for _, w := range waiting {
		if w.Kind == b.kind && w.Identity == b.identity {
			row = w
			break
		}
	}
	if row.ID == 0 {
		id, c, err := e.approvalAuthority().Request(ctx, e.store.Store, approval.Act{Kind: b.kind, ID: b.identity}, 0)
		if err != nil {
			return nil, fmt.Errorf("ask for the approval: %w", err)
		}
		row = store.ApprovalRequest{ID: id, Kind: b.kind, Identity: b.identity, Nonce: c.Nonce, Expires: c.Expires}
		wanted := ApprovalWanted(row)
		err = e.store.Update(ctx, func(tx *sql.Tx) error {
			_, err := appendEventTx(tx, evApprovalWanted, b.project, 0, 0,
				eventPayload{Status: "waiting", Reason: wanted, Request: id}, now)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("record the approval request: %w", err)
		}
	}
	msg, err := approval.RequestChallenge(row).Message()
	if err != nil {
		return nil, err
	}
	keys, err := approval.NewStore(e.cfg.ApprovalKeys).List()
	if err != nil {
		return nil, err
	}
	return &PendingApproval{
		RequestID: row.ID,
		Act:       DescribeGatedAct(row.Kind, row.Identity),
		Challenge: row.Nonce,
		Message:   string(msg),
		Namespace: approval.Namespace,
		Expires:   row.Expires,
		NoKey:     len(keys) == 0,
		Wanted:    ApprovalWanted(row),
	}, nil
}

// ApprovalWanted is the one line every surface prints for a request that
// waits: the needs-you queue of `brigada status` and the foreman's, the
// notification, and an API refusal. It names the act and the challenge
// and says the signature comes from outside, not from a token.
func ApprovalWanted(r store.ApprovalRequest) string {
	return fmt.Sprintf("signature wanted out of band to %s (request #%d, challenge %s, expires %s)",
		DescribeGatedAct(r.Kind, r.Identity), r.ID, r.Nonce, r.Expires.UTC().Format(time.RFC3339))
}

// DescribeGatedAct words an act from its challenge's kind and identity.
func DescribeGatedAct(kind, identity string) string {
	verb, f := parseGatedIdentity(identity)
	stop := ""
	if f["stop"] == 1 {
		stop = " and stop asking"
	}
	switch kind {
	case approval.KindPlan:
		return fmt.Sprintf("%s plan #%d at version %d%s", verb, f["plan"], f["v"], stop)
	case approval.KindTask:
		return fmt.Sprintf("%s the landing of task #%d%s", strings.TrimPrefix(verb, "land-"), f["task"], stop)
	case approval.KindPush:
		return fmt.Sprintf("%s the push of project #%d", verb, f["project"])
	}
	return kind + " " + identity
}

// parseGatedIdentity splits "verb:key=n:key=n" back into its parts; a
// field that is not a number reads as zero.
func parseGatedIdentity(identity string) (string, map[string]int64) {
	parts := strings.Split(identity, ":")
	f := map[string]int64{}
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		f[k] = n
	}
	return parts[0], f
}

// statusApprovalItemsTx lists the pending requests as needs-you entries
// (§10): one line each, ID the request's, naming the act and the
// challenge. project scopes the queue to one project, 0 to all.
func statusApprovalItemsTx(tx *sql.Tx, project int64, now time.Time) ([]NeedsYouItem, error) {
	rows, err := store.WaitingApprovalRequestsTx(tx, now)
	if err != nil {
		return nil, fmt.Errorf("list the pending approvals: %w", err)
	}
	var items []NeedsYouItem
	for _, r := range rows {
		_, f := parseGatedIdentity(r.Identity)
		if project != 0 && f["project"] != project {
			continue
		}
		items = append(items, NeedsYouItem{
			Kind:   "approval",
			ID:     r.ID,
			State:  store.ApprovalWaiting,
			Title:  DescribeGatedAct(r.Kind, r.Identity),
			Reason: ApprovalWanted(r),
		})
	}
	return items, nil
}
