package lifecycle_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestRunFlushesNotificationsBeforeIdleExit runs the engine role
// in-process (§13): the §10 hook is the whole notification story so far,
// and an idle exit must call it before the socket and the lock go.
func TestRunFlushesNotificationsBeforeIdleExit(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	p := lifecycle.PathsOf(state)
	flushed := make(chan struct{}, 1)
	err := lifecycle.Run(context.Background(), lifecycle.Config{
		StateDir: state,
		Idle:     400 * time.Millisecond,
		IdleTick: 50 * time.Millisecond,
		FlushNotifications: func(context.Context) error {
			flushed <- struct{}{}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run the engine: %v", err)
	}
	select {
	case <-flushed:
	default:
		t.Error("the idle exit did not flush the notifications")
	}
	if _, err := os.Stat(p.Socket); !os.IsNotExist(err) {
		t.Errorf("the socket survived the idle exit: %v", err)
	}
	if pid := lifecycle.EnginePid(p.Lock); pid != 0 {
		t.Errorf("the lock survived the idle exit, held by pid %d", pid)
	}
	if _, err := os.Stat(p.Tokens); err != nil {
		t.Errorf("the token file was not created: %v", err)
	}
}
