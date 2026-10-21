package testsupport

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// storeTemplate is a fully migrated, still empty store captured as one
// file's bytes, built by the first caller in a process. store.Open
// applies every embedded migration on open, so a suite with a store per
// test parses the whole schema again for each test; under the race
// detector that parse is most of a suite's time. Each test instead
// starts from a copy of this template and opens a store that is already
// migrated.
var (
	storeTemplateOnce sync.Once
	storeTemplate     []byte
	storeTemplateErr  error
)

// OpenStore opens a private, fully migrated store for one test: a copy
// of the process's template in the test's own temp dir, removed when
// the test ends. Unlike store.Open's ":memory:", the store is a file,
// so the pool may hand the test several connections.
func OpenStore(t testing.TB) *store.Store {
	t.Helper()
	st, err := openStore(t)
	if err != nil {
		t.Fatalf("open a migrated store for the test: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func openStore(t testing.TB) (*store.Store, error) {
	storeTemplateOnce.Do(buildStoreTemplate)
	if storeTemplateErr != nil {
		return nil, storeTemplateErr
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	if err := os.WriteFile(path, storeTemplate, 0o644); err != nil {
		return nil, err
	}
	return store.Open(context.Background(), path)
}

// buildStoreTemplate migrates a throwaway store, closes it and keeps
// its bytes. A clean SQLite close checkpoints and drops the write-ahead
// log, so the one file is the whole database.
func buildStoreTemplate() {
	dir, err := os.MkdirTemp("", "bs-store-template-")
	if err != nil {
		storeTemplateErr = err
		return
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(context.Background(), filepath.Join(dir, "template.db"))
	if err != nil {
		storeTemplateErr = err
		return
	}
	path := st.Path()
	if err := st.Close(); err != nil {
		storeTemplateErr = err
		return
	}
	storeTemplate, storeTemplateErr = os.ReadFile(path)
}
