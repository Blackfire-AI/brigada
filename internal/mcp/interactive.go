package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/projecturl"
	"github.com/google/uuid"
)

// Interactive owns one process's mode A identity and in-memory hold secret.
// Bootstrap is only used for project discovery, scoped reads and acquisition;
// mutating tools always receive the engine-minted hold credential.
type receivedKey struct{}

type Interactive struct {
	acquiredAt     time.Time
	trafficMu      sync.Mutex
	pending        int
	pendingSession string
	shared         atomic.Bool
	Bootstrap      *api.Client
	RepoURL        string // explicit override
	CWD            string
	BridgeInstance string
	SessionID      string
	SessionSource  string
	ObservedAt     time.Time
	Label          string

	mu           sync.Mutex
	first        bool
	candidates   []string
	project      string
	projectID    int64
	held         *api.Client
	hold         api.AcquireResponse
	lost         bool
	isolatedLost bool
	hint         string
	hook         *hookServer
}

// NewInteractive mints the bridge identity at process start. A harness session
// is an observation, never the identity or the credential of a hold.
func NewInteractive(c *api.Client, repo, cwd, claudeSession string) *Interactive {
	b := &Interactive{Bootstrap: c, RepoURL: repo, CWD: cwd, BridgeInstance: uuid.NewString(), Label: "your session"}
	if claudeSession != "" {
		b.SessionID, b.SessionSource, b.ObservedAt = claudeSession, "env", time.Now().UTC()
		b.Label = "Claude Code"
	}
	return b
}

// codexMetadata accepts the JSON object and JSON-encoded object forms of the
// MCP extension. Only workspace remote fields are used as project selectors.
func codexMetadata(meta json.RawMessage) (string, []string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return "", nil, false
	}
	raw, ok := m["x-codex-turn-metadata"]
	if !ok {
		return "", nil, false
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		raw = []byte(encoded)
	}
	var turn map[string]json.RawMessage
	if json.Unmarshal(raw, &turn) != nil {
		return "", nil, true
	}
	var session string
	_ = json.Unmarshal(turn["session_id"], &session)
	var remotes []string
	// Remote fields are selectors. Workspace path strings and other metadata
	// (model, effort, sandbox, cwd) are never interpreted as repository URLs.
	var remoteValues func(json.RawMessage)
	remoteValues = func(raw json.RawMessage) {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			remotes = append(remotes, value)
			return
		}
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) == nil {
			for _, v := range values {
				remoteValues(v)
			}
			return
		}
		var record map[string]json.RawMessage
		if json.Unmarshal(raw, &record) == nil {
			for _, key := range []string{"url", "remote_url", "fetch_url", "urls", "git_remote_urls", "remote_urls"} {
				if v, ok := record[key]; ok {
					remoteValues(v)
				}
			}
		}
	}
	var workspaceRecords func(json.RawMessage)
	workspaceRecords = func(raw json.RawMessage) {
		var records []json.RawMessage
		if json.Unmarshal(raw, &records) == nil {
			for _, record := range records {
				workspaceRecords(record)
			}
			return
		}
		var record map[string]json.RawMessage
		if json.Unmarshal(raw, &record) != nil {
			return
		}
		for _, key := range []string{"git_remote_urls", "remote_urls", "workspace_git_remote_urls", "workspace_git_remotes", "workspace_remotes"} {
			if v, ok := record[key]; ok {
				remoteValues(v)
			}
		}
		for _, key := range []string{"workspace_roots", "workspaces"} {
			if v, ok := record[key]; ok {
				workspaceRecords(v)
			}
		}
	}
	workspaceRecords(raw)
	return session, remotes, true
}

func gitRootOrigin(ctx context.Context, cwd string) (string, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	root, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("cannot find the repository root; run brigada init in the repository")
	}
	origin, err := exec.CommandContext(ctx, "git", "-C", strings.TrimSpace(string(root)), "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("repository has no origin; run brigada init to select its project")
	}
	return strings.TrimSpace(string(origin)), nil
}

// observeFirst records the first tools/call, even if it names an unknown tool.
// The caller holds mu. Later metadata cannot redirect this process.
func (b *Interactive) observeFirst(ctx context.Context, meta json.RawMessage) {
	if !b.first {
		b.first = true
		session, urls, codex := codexMetadata(meta)
		if codex && b.SessionSource != "env" {
			b.SessionID, b.SessionSource, b.ObservedAt = session, "request_meta", time.Now().UTC()
			b.Label = "Codex"
		}
		switch {
		case b.RepoURL != "":
			b.candidates = []string{b.RepoURL}
		case codex:
			b.candidates = urls
		default:
			origin, err := gitRootOrigin(ctx, b.CWD)
			if err == nil {
				b.candidates = []string{origin}
			}
		}
	}
}

// selectProject resolves only the selectors frozen by observeFirst.
func (b *Interactive) selectProject(ctx context.Context, meta json.RawMessage) error {
	b.observeFirst(ctx, meta)
	if b.project != "" {
		return nil
	}
	const help = "no single canonical project selected; run brigada init in the repository, then reconnect (or pass an explicit repo URL to brigada mcp --foreman)"
	if len(b.candidates) == 0 {
		return errors.New(help)
	}
	keys := map[string]bool{}
	for _, raw := range b.candidates {
		key, err := projecturl.Canonical(raw)
		if err != nil {
			return errors.New(help)
		}
		keys[key] = true
	}
	projects, err := b.Bootstrap.Projects(ctx)
	if err != nil {
		return fmt.Errorf("select project: %w; run brigada init to check setup", err)
	}
	matches := 0
	for _, p := range projects {
		key, err := projecturl.Canonical(p.RepoURL)
		if err == nil && keys[key] {
			b.project, b.projectID = p.RepoURL, p.ProjectID
			matches++
		}
	}
	if matches != 1 {
		b.project, b.projectID = "", 0
		return errors.New(help)
	}
	return nil
}

func (b *Interactive) acquire(ctx context.Context) (string, error) {
	if b.isolatedLost || b.shared.Load() {
		return "", errors.New("hold_lost: this bridge saw unattributable shared traffic; reconnect with one conversation")
	}
	h, err := b.Bootstrap.AcquireHold(ctx, api.HoldAcquireRequest{
		RepoURL: b.project, BridgeInstance: b.BridgeInstance, Label: b.Label, SessionHint: b.hint,
		HarnessSessionID: b.SessionID, HarnessSessionSource: b.SessionSource, HarnessSessionObservedAt: b.ObservedAt,
	})
	if err != nil {
		return "", err
	}
	b.held = b.Bootstrap.HoldClient(h.Credential, h.Fence)
	h.Credential = ""
	b.hold, b.lost = h, false
	b.acquiredAt = time.Now()
	if b.hook != nil {
		if err := b.hook.publish(b); err != nil {
			b.hook.log(err)
		}
	}
	// The credential is never returned to the model or written to disk.
	data, err := json.Marshal(h.Digest)
	return string(data), err
}

func (b *Interactive) yield(ctx context.Context) error {
	if b.held == nil {
		return nil
	}
	c := b.held
	b.held, b.hold = nil, api.AcquireResponse{}
	if b.hook != nil {
		b.hook.unpublish()
	}
	return c.YieldHold(ctx)
}

func (b *Interactive) call(ctx context.Context, tool Tool, args, meta json.RawMessage) (string, error) {
	b.mu.Lock()
	if b.shared.Load() {
		_ = b.yield(ctx)
		b.lost, b.isolatedLost = true, true
	}
	if err := b.selectProject(ctx, meta); err != nil {
		b.mu.Unlock()
		return "", err
	}
	session, _, codex := codexMetadata(meta)
	var a struct {
		SessionHint string `json:"session_hint"`
	}
	_ = json.Unmarshal(args, &a)
	if (codex && session != b.SessionID) || (b.hint != "" && a.SessionHint != "" && a.SessionHint != b.hint) {
		_ = b.yield(ctx)
		b.lost, b.isolatedLost = true, true
	}
	if a.SessionHint != "" {
		b.hint = a.SessionHint
	}
	if b.isolatedLost || (b.lost && tool.Name != "hold_acquire") {
		b.mu.Unlock()
		return "", errors.New("hold_lost: the bridge lost authority; call hold_acquire, read the digest and decide again; shared conversations must reconnect separately")
	}
	readOnly := tool.Name == "status" || tool.Name == "history" || tool.Name == "capacity" || tool.Name == "stats"
	received, _ := ctx.Value(receivedKey{}).(time.Time)
	if !readOnly && tool.Name != "hold_acquire" && !received.IsZero() && received.Before(b.acquiredAt) {
		b.mu.Unlock()
		return "", errors.New("hold_lost: this call was queued before acquire completed; read the digest and decide again")
	}
	switch tool.Name {
	case "hold_acquire":
		text, err := b.acquire(ctx)
		b.mu.Unlock()
		return text, err
	case "hold_yield":
		err := b.yield(ctx)
		b.mu.Unlock()
		return "hold yielded", err
	}
	digest := ""
	if !readOnly && b.held == nil {
		var err error
		digest, err = b.acquire(ctx)
		if err != nil {
			b.mu.Unlock()
			return "", err
		}
	}
	if b.shared.Load() {
		_ = b.yield(ctx)
		b.lost, b.isolatedLost = true, true
		b.mu.Unlock()
		return "", errors.New("hold_lost: overlapping calls could not be attributed; reconnect with one conversation")
	}
	c := b.held
	if c == nil {
		c = b.Bootstrap.ProjectReader(b.projectID)
	}
	b.mu.Unlock()
	text, err := tool.Call(ctx, c, args)
	var se *api.StatusError
	if errors.As(err, &se) && se.APICode == "stale_hold" {
		b.mu.Lock()
		// Never retry this call, and do not poison a newer explicit acquire.
		if b.held == c {
			b.held = nil
			b.lost = true
			if b.hook != nil {
				b.hook.unpublish()
			}
		}
		b.mu.Unlock()
	}
	if b.shared.Load() {
		return "", errors.New("hold_lost: overlapping calls could not be attributed; reconnect with one conversation and read the engine journal")
	}
	if digest != "" && err == nil {
		text = "hold acquired; current state digest:\n" + digest + "\n\n" + text
	}
	return text, err
}

// run renews only existing holds. A transport failure is retried inside the
// lease; a rejection or an elapsed lease drops authority until explicit acquire.
func (b *Interactive) run(ctx context.Context) {
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	var next time.Time
	var current int64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			b.mu.Lock()
			if b.held == nil {
				next = time.Time{}
				b.mu.Unlock()
				continue
			}
			if current != b.hold.HoldID {
				current = b.hold.HoldID
				next = time.Time{}
			}
			remaining := time.Until(b.hold.LeaseExpires)
			if remaining <= 0 {
				b.held = nil
				b.lost = true
				if b.hook != nil {
					b.hook.unpublish()
				}
				b.mu.Unlock()
				continue
			}
			if next.IsZero() {
				next = now.Add(remaining / 3)
			}
			if now.Before(next) {
				b.mu.Unlock()
				continue
			}
			timeout := min(remaining, 5*time.Second)
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			lease, err := b.held.HeartbeatHold(callCtx)
			cancel()
			var se *api.StatusError
			switch {
			case err == nil:
				b.hold.LeaseExpires = lease
				next = time.Now().Add(time.Until(lease) / 3)
			case errors.As(err, &se) && se.IsClientError(), !time.Now().Before(b.hold.LeaseExpires):
				b.held = nil
				b.lost = true
				if b.hook != nil {
					b.hook.unpublish()
				}
			default:
				next = time.Now().Add(min(time.Second, remaining/4))
			}
			b.mu.Unlock()
		}
	}
}

func (b *Interactive) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b.mu.Lock()
	_ = b.yield(ctx)
	b.mu.Unlock()
	if b.hook != nil {
		b.hook.close()
	}
}

// incoming observes overlap at the transport, before serial dispatch can hide
// it. Only verified per-call Codex metadata attributes overlapping calls.
func (b *Interactive) incoming(line []byte) {
	var req request
	if json.Unmarshal(line, &req) != nil || req.Method != "tools/call" || len(req.ID) == 0 || string(req.ID) == "null" {
		return
	}
	var p struct {
		Meta json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal(req.Params, &p)
	session, _, verified := codexMetadata(p.Meta)
	b.trafficMu.Lock()
	shared := b.pending > 0 && (!verified || session == "" || session != b.pendingSession)
	if b.pending == 0 {
		b.pendingSession = session
	}
	b.pending++
	b.trafficMu.Unlock()
	if shared {
		b.shared.Store(true)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			b.mu.Lock()
			_ = b.yield(ctx)
			b.lost, b.isolatedLost = true, true
			b.mu.Unlock()
		}()
	}
}
func (b *Interactive) answered(line []byte) {
	var req request
	if json.Unmarshal(line, &req) != nil || req.Method != "tools/call" || len(req.ID) == 0 || string(req.ID) == "null" {
		return
	}
	b.trafficMu.Lock()
	b.pending--
	b.trafficMu.Unlock()
}
