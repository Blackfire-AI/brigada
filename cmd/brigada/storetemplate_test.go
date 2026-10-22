package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// The migrated-store template: store.Open's migrations cost about a
// second under -race, so a test that starts an engine on a fresh state
// dir drops one migrated file at the store path first. Open on the copy
// finds the schema at the latest version and applies nothing.
var (
	storeTemplateOnce sync.Once
	storeTemplate     []byte
	storeTemplateErr  error
)

func migratedStoreBytes() ([]byte, error) {
	storeTemplateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cmd-store-template")
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

// seedMigratedStore writes a fully migrated, empty store at path, for an
// engine or test to open.
func seedMigratedStore(t testing.TB, path string) {
	t.Helper()
	data, err := migratedStoreBytes()
	if err != nil {
		t.Fatalf("build the migrated store template: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write the store from the template: %v", err)
	}
}

// openFreshStore opens a fresh, fully migrated store at path.
func openFreshStore(t testing.TB, path string) *store.Store {
	t.Helper()
	seedMigratedStore(t, path)
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return st
}
