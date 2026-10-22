package api

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// The migrated-store template: store.Open's migrations cost about a
// second under -race, so every test that wants a fresh store copies one
// migrated file instead of migrating again. Open on the copy finds the
// schema at the latest version and applies nothing.
var (
	storeTemplateOnce sync.Once
	storeTemplate     []byte
	storeTemplateErr  error
)

func migratedStoreBytes() ([]byte, error) {
	storeTemplateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "api-store-template")
		if err != nil {
			storeTemplateErr = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "store.db")
		st, err := store.Open(context.Background(), path)
		if err != nil {
			storeTemplateErr = err
			return
		}
		// Closing the last handle checkpoints the WAL into the main file.
		if err := st.Close(); err != nil {
			storeTemplateErr = err
			return
		}
		storeTemplate, storeTemplateErr = os.ReadFile(path)
	})
	return storeTemplate, storeTemplateErr
}

// openFreshStore opens a fresh, fully migrated store at path.
func openFreshStore(t testing.TB, path string) *store.Store {
	t.Helper()
	data, err := migratedStoreBytes()
	if err != nil {
		t.Fatalf("build the migrated store template: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write the store from the template: %v", err)
	}
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}
