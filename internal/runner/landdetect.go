package runner

import (
	"context"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
	"github.com/Blackfire-AI/brigada/internal/landing"
)

// ReportLanded scans the branch a freshly refreshed foreman checkout
// reads — the land target when one is set, else the default branch — for
// the trailers the landing record scan reads (§7.1): Brigada-Task
// trailers, every commit carrying one reported newest first per task
// with its Brigada-Branch trailer's branch and head — a landing made
// outside the land job, a hand squash or a brigada land in the user's
// clone — and Brigada-Sync trailers, every commit carrying one reported
// with its default branch and head — a sync merge the user pushed to the
// target by hand under gate_push never. The engine validates each
// candidate before it records anything. It is called from a headless
// turn and from attach, the two places the checkout is refreshed; a
// failure to scan or report is returned, and the callers treat it as a
// warning, never a turn or attach failure.
func ReportLanded(ctx context.Context, client *api.Client, repoURL, branch string, checkout foreman.Checkout) error {
	landed, err := landing.LandedCommits(ctx, checkout.Path, "HEAD")
	if err != nil {
		return err
	}
	if len(landed) > 0 {
		recs := make([]engine.LandedCommit, 0, len(landed))
		for id, candidates := range landed {
			for _, c := range candidates {
				recs = append(recs, engine.LandedCommit{TaskID: id, Commit: c.SHA, Branch: c.Branch, Head: c.Head})
			}
		}
		if err := client.RecordLanded(ctx, repoURL, branch, recs); err != nil {
			return err
		}
	}
	syncs, err := landing.SyncCommits(ctx, checkout.Path, "HEAD")
	if err != nil {
		return err
	}
	if len(syncs) == 0 {
		return nil
	}
	srecs := make([]engine.SyncCommit, 0, len(syncs))
	for _, c := range syncs {
		srecs = append(srecs, engine.SyncCommit{Commit: c.SHA, Default: c.Default, Head: c.Head})
	}
	return client.RecordSync(ctx, repoURL, branch, srecs)
}
