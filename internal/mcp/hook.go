package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HookBinding is an immutable snapshot of the bridge and hold a Stop hook may
// yield. There is no credential here. SessionID only correlates; HarnessPID
// binds discovery to the harness process that started this bridge.
type HookBinding struct {
	BridgeInstance string `json:"bridge_instance"`
	HoldID         int64  `json:"hold_id"`
	Fence          int64  `json:"fence"`
	SessionID      string `json:"session_id,omitempty"`
	HarnessPID     int    `json:"harness_pid"`
	Socket         string `json:"socket"`
}

type hookServer struct {
	listener         net.Listener
	stateDir, record string
	binding          HookBinding
}

// StartHook enables the local hook rendezvous. The hook talks to this bridge;
// only this bridge can present its memory-only hold credential to the engine.
func (b *Interactive) StartHook(stateDir string, harnessPID int) error {
	dir := filepath.Join(stateDir, "bridges")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// No longer than the engine's brigada.sock: long state paths already
	// validated by lifecycle must also work for the hook rendezvous.
	socket := filepath.Join(stateDir, "b"+strings.ReplaceAll(b.BridgeInstance, "-", "")[:10])
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("start hold hook bridge: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		listener.Close()
		return err
	}
	h := &hookServer{listener: listener, stateDir: stateDir, record: filepath.Join(dir, b.BridgeInstance+".json"),
		binding: HookBinding{BridgeInstance: b.BridgeInstance, HarnessPID: harnessPID, Socket: socket}}
	b.hook = h
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				var ref HookBinding
				err := json.NewDecoder(conn).Decode(&ref)
				if err == nil {
					b.mu.Lock()
					switch {
					case ref.BridgeInstance != b.BridgeInstance || b.held == nil || ref.HoldID != b.hold.HoldID || ref.Fence != b.hold.Fence:
						err = errors.New("stale_hold: hook binding no longer holds authority")
					case ref.SessionID != "" && ref.SessionID != b.SessionID:
						err = errors.New("hook session does not correlate with this bridge")
					default:
						ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
						err = b.yield(ctx)
						cancel()
					}
					b.mu.Unlock()
				}
				message := ""
				if err != nil {
					message = err.Error()
					h.log(err)
				}
				_ = json.NewEncoder(conn).Encode(struct {
					Error string `json:"error,omitempty"`
				}{message})
			}()
		}
	}()
	return nil
}

func (h *hookServer) publish(b *Interactive) error {
	ref := h.binding
	ref.HoldID, ref.Fence, ref.SessionID = b.hold.HoldID, b.hold.Fence, b.SessionID
	data, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	// Atomic publication: a hook sees either the old fence or the new one.
	tmp, err := os.CreateTemp(filepath.Dir(h.record), ".binding-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), h.record)
}
func (h *hookServer) unpublish()    { _ = os.Remove(h.record) }
func (h *hookServer) log(err error) { LogHookFailure(h.stateDir, err) }
func (h *hookServer) close()        { h.unpublish(); _ = h.listener.Close() }

// LogHookFailure is deliberately best effort: diagnostics must never break a
// harness turn, even when the state directory itself is unavailable.
func LogHookFailure(stateDir string, err error) {
	if err == nil {
		return
	}
	if os.MkdirAll(stateDir, 0o700) != nil {
		return
	}
	f, openErr := os.OpenFile(filepath.Join(stateDir, "hold-yield.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if openErr != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s hold yield: %v\n", time.Now().UTC().Format(time.RFC3339Nano), err)
}

// YieldFromHook finds a unique bridge started by an ancestor harness. A session
// id by itself never selects a bridge. Multiple bridge processes in that harness
// fail closed; a stale socket or fence never falls back to a different bridge.
func YieldFromHook(ctx context.Context, stateDir, session string) error {
	entries, err := os.ReadDir(filepath.Join(stateDir, "bridges"))
	if err != nil {
		return fmt.Errorf("no bridge binding for hold hook: %w", err)
	}
	// Snapshot the references once. Never refresh a stale reference to a newer
	// fence or retry against a different bridge after a release failure.
	var refs []HookBinding
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, "bridges", entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var ref HookBinding
		if err := json.Unmarshal(data, &ref); err != nil {
			return fmt.Errorf("invalid bridge binding: %w", err)
		}
		if session == "" || ref.SessionID == session {
			refs = append(refs, ref)
		}
	}
	var selected *HookBinding
	seen := map[int]bool{}
	for pid, depth := os.Getpid(), 0; pid > 1 && depth < 64; depth++ {
		if seen[pid] {
			break
		}
		seen[pid] = true
		for i := range refs {
			if refs[i].HarnessPID != pid {
				continue
			}
			if selected != nil {
				return errors.New("ambiguous bridge binding for hold hook; reconnect with one conversation")
			}
			selected = &refs[i]
		}
		if selected != nil {
			break
		}
		if pid == os.Getpid() {
			pid = os.Getppid()
			continue
		}
		out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return fmt.Errorf("find harness process for hold hook: %w", err)
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return fmt.Errorf("read harness parent for hold hook: %w", err)
		}
	}
	if selected == nil {
		return errors.New("no matching bound bridge for hold hook; reconnect the foreman bridge")
	}
	return yieldBinding(ctx, *selected)
}

func yieldBinding(ctx context.Context, ref HookBinding) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", ref.Socket)
	if err != nil {
		return fmt.Errorf("reach bound bridge for hold hook: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	if t, ok := ctx.Deadline(); ok && t.Before(deadline) {
		deadline = t
	}
	_ = conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(ref); err != nil {
		return err
	}
	var result struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(conn).Decode(&result); err != nil {
		return err
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}
