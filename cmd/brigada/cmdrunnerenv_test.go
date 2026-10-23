package main

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// The start path resolves each offered harness once, however many models
// it offers, and only the offered harnesses.
func TestResolveRunnerHarnessEnv(t *testing.T) {
	reads := 0
	run := func(string, ...string) ([]byte, error) {
		reads++
		return []byte("value-from-keychain\n"), nil
	}
	refs := map[string]map[string]string{
		"dsh":   {"SOME_VAR": "keychain:brigada-item"},
		"codex": {"OTHER_VAR": "keychain:other-item"},
	}
	adverts := []api.Agent{{Harness: "dsh", Model: "a"}, {Harness: "dsh", Model: "b"}}
	got, err := resolveRunnerHarnessEnv(refs, adverts, run)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(got["dsh"]) != 1 || got["dsh"][0] != "SOME_VAR=value-from-keychain" || len(got["codex"]) != 0 {
		t.Errorf("reads %d, resolved %v", reads, got)
	}
}

func TestResolveRunnerHarnessEnvErrorHasNoValue(t *testing.T) {
	run := func(string, ...string) ([]byte, error) {
		return []byte("value-from-keychain"), errMock("value-from-keychain")
	}
	refs := map[string]map[string]string{"dsh": {"SOME_VAR": "keychain:brigada-item"}}
	_, err := resolveRunnerHarnessEnv(refs, []api.Agent{{Harness: "dsh"}}, run)
	if err == nil || strings.Contains(err.Error(), "value-from-keychain") {
		t.Errorf("err = %v", err)
	}
}

type errMock string

func (e errMock) Error() string { return string(e) }
