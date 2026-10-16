package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// statsReader is the one call the stats command needs from the API
// client; tests stub it.
type statsReader interface {
	Stats(ctx context.Context, projectID int64) ([]engine.ProjectStats, error)
}

// runStats is the client's stats (§8): what the outcome record has
// learned, a plain table per repo — which agent wins which class, with
// how many attempts behind it.
func runStats(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stats")
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code := common.parse(stderr, args); code != 0 {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "stats takes no arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	return printStats(ctx, client, stdout, stderr)
}

// printStats reads the stats from the client and prints the tables.
func printStats(ctx context.Context, client statsReader, stdout, stderr io.Writer) int {
	stats, err := client.Stats(ctx, 0)
	if err != nil {
		return fail(stderr, err)
	}
	if err := renderStats(stdout, stats); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// statsConfidence names how much to trust a score by the attempts
// behind it (§8): none with no record, starting for a few, enough from
// five on.
func statsConfidence(attempts int) string {
	switch {
	case attempts <= 0:
		return "none"
	case attempts < 5:
		return "starting"
	default:
		return "enough"
	}
}

// renderStats prints the stats as one aligned table per project: the
// agent, the class, attempts, successes, the rate (the score, prior
// included) and the confidence word. An agent whose runner has not been
// seen lately is marked stale; a registered agent with no record shows
// at attempts 0, confidence none.
func renderStats(w io.Writer, stats []engine.ProjectStats) error {
	for i, p := range stats {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, p.RepoURL)
		if len(p.Rows) == 0 {
			fmt.Fprintln(w, "  no agents registered")
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  AGENT\tCLASS\tATTEMPTS\tSUCCESSES\tRATE\tCONFIDENCE")
		for _, r := range p.Rows {
			agent := agentName(r.Harness, r.Model, r.Effort)
			if !r.Live {
				agent += " (stale)"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%.2f\t%s\n",
				agent, r.Class, r.Attempts, r.Successes, r.Score, statsConfidence(r.Attempts))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}
