package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/caps"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// callCapacity renders /v1/capacity as compact text (§12.2): one line
// per agent — its effective cap and the layer that sets it, its running
// jobs, its quota state and its cost weight — then one line per layer:
// cap, use and queue depth for each runner, each account, each capped
// model, each active plan of the project and global. That is the read
// the foreman plans from (§4.5): how wide the task graph can be, when
// to sequence instead of fanning out, and which agents are cooling or
// broken. The tool takes no arguments; a foreman token's scope is its
// own project's plans (§12.1).
func callCapacity(ctx context.Context, c *api.Client, _ json.RawMessage) (string, error) {
	snap, err := c.Capacity(ctx, 0)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, a := range snap.Agents {
		cap := capText(a.Cap)
		if a.Cap != nil {
			cap += " (" + a.CapLayer + ")"
		}
		quota := a.Quota
		if a.Quota == "cooling" {
			quota += " until " + a.CoolingUntil
		}
		tools := ""
		if a.Tools != "" {
			tools = " tools=" + a.Tools
		}
		fmt.Fprintf(&b, "agent %d %s/%s account=%s cap=%s running=%d quota=%s cost=%d%s\n",
			a.ID, a.Harness, a.Model, a.Account, cap, a.Running, quota, a.Cost, tools)
	}
	for _, l := range snap.Layers {
		name := l.Layer
		if key := layerKey(l); key != "" {
			name += " " + key
		}
		fmt.Fprintf(&b, "%s cap=%s use=%d queue=%d\n", name, capText(l.Cap), l.Use, l.Queue)
	}
	for _, h := range snap.Headroom {
		fmt.Fprintf(&b, "headroom project %d account=%s cap=%s\n", h.ProjectID, h.Account, capText(h.Cap))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// capText renders one cap for the capacity lines: "none" for an
// uncapped layer, the number for a cap — 0 stays "0", which is a layer
// capped at nothing, not an uncapped one (§8.1).
func capText(cap *int) string {
	if cap == nil {
		return "none"
	}
	return strconv.Itoa(*cap)
}

// layerKey names a layer line's instance: the runner or plan id, the
// account or model label — empty for global.
func layerKey(l engine.CapacityLayer) string {
	switch l.Layer {
	case caps.LayerRunner:
		return strconv.FormatInt(l.RunnerID, 10)
	case caps.LayerAccount:
		return l.Account
	case caps.LayerModel:
		return l.Model
	case caps.LayerPlan:
		return strconv.FormatInt(l.PlanID, 10)
	default:
		return ""
	}
}
