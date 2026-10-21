package runner

import (
	"context"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
	"github.com/Blackfire-AI/brigada/internal/landing"
)

// ReportLanded scans a freshly refreshed foreman checkout's default
// branch for Brigada-Task trailers and reports each task it finds to the
// engine as landed (§7.1): a landing made outside the land job — a hand
// squash, or a brigada land in the user's clone. It is called from a
// headless turn and from attach, the two places the checkout is
// refreshed; a failure to scan or report is returned, and the callers
// treat it as a warning, never a turn or attach failure.
func ReportLanded(ctx context.Context, client *api.Client, repoURL, branch string, checkout foreman.Checkout) error {
	landed, err := landing.LandedCommits(ctx, checkout.Path, "HEAD")
	if err != nil {
		return err
	}
	if len(landed) == 0 {
		return nil
	}
	recs := make([]engine.LandedCommit, 0, len(landed))
	for id, sha := range landed {
		recs = append(recs, engine.LandedCommit{TaskID: id, Commit: sha})
	}
	return client.RecordLanded(ctx, repoURL, branch, recs)
}
