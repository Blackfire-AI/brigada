#!/usr/bin/env bash
# Release check for mode A (GitHub issue #26): run by a person before a
# release, never by CI.
#
# Needs on PATH: brigada (the build under test), git, go, jq, claude, codex.
# Claude Code and Codex must already be signed in.
#
# Isolation: everything happens in one temp dir. brigada's state and config
# live in a temp XDG tree, reached only through a `brigada` shim placed first
# on PATH. The shim sets XDG_STATE_HOME and XDG_CONFIG_HOME for brigada alone,
# so the claude and codex runs keep their normal environment and sign-in,
# while the bare `brigada mcp --foreman` bridge from .mcp.json (resolved via
# PATH) still talks to the temp engine and not to yours.
set -euo pipefail

for tool in brigada git go jq claude codex; do
	command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 2; }
done

REAL_BRIGADA=$(command -v brigada)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/brigada-release-check.XXXXXX")
ORIGIN=$TMP/origin.git
REPO=$TMP/repo
SHIM=$TMP/bin
INIT_DONE=0
FAILS=0

cleanup() {
	local rc=$?
	set +e
	if [ "$INIT_DONE" = 1 ] && [ -d "$REPO" ]; then
		(cd "$REPO" && brigada remove --yes --cancel --delete-config >/dev/null 2>&1)
	fi
	rm -rf "$TMP"
	exit "$rc"
}
trap cleanup EXIT

mkdir -p "$SHIM" "$TMP/state" "$TMP/config"
cat >"$SHIM/brigada" <<SH
#!/bin/sh
XDG_STATE_HOME="$TMP/state" XDG_CONFIG_HOME="$TMP/config" exec "$REAL_BRIGADA" "\$@"
SH
chmod +x "$SHIM/brigada"
export PATH="$SHIM:$PATH"

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }

export GIT_AUTHOR_NAME=release-check GIT_AUTHOR_EMAIL=release-check@example.invalid
export GIT_COMMITTER_NAME=release-check GIT_COMMITTER_EMAIL=release-check@example.invalid

# Fresh repository: a Go module with one failing test, a local bare origin.
git init -q --bare -b main "$ORIGIN"
mkdir "$REPO"
cd "$REPO"
git init -q -b main
git remote add origin "$ORIGIN"
printf 'module example.com/releasecheck\n\ngo 1.21\n' >go.mod
printf 'package releasecheck\n\nfunc Add(a, b int) int { return a - b }\n' >add.go
cat >add_test.go <<'GO'
package releasecheck

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2, 3) = %d, want 5", got)
	}
}
GO
git add -A
git commit -q -m "initial: a failing test"
git push -q origin main

# Non-interactive init with both CLIs signed in.
brigada init --yes --check "go test ./..." --parallel 1 \
	--agent harness=claude --agent harness=codex \
	--signed-in claude,codex --foreman claude/sonnet@medium
INIT_DONE=1

# Init leaves its wiring uncommitted, and task submit builds the worker's
# worktree from origin: commit and push it so the router reaches the worker.
git add -A
git commit -q -m "chore: brigada wiring"
git push -q origin main

baseline_head=$(git rev-parse HEAD)
baseline_status=$(git status --porcelain)

# Shared by checks 1 and 2: planned or asked, no edits, no commits.
# $1 label, $2 file of tool-call names (one per line), $3 file of the
# assistant's message text (agent messages or the final result only).
verify_no_edit() {
	local label=$1 tools=$2 text=$3 planned=""
	if grep -Eiq 'plan_create' "$tools"; then
		planned="called plan_create"
	elif grep -Eiq 'approv.*plan|plan.*approv' "$text"; then
		planned="asked the user to approve a plan"
	fi
	if [ -n "$planned" ]; then
		pass "$label: $planned"
	else
		fail "$label: neither plan_create nor a request to approve a plan"
	fi
	if [ "$(git rev-parse HEAD)" != "$baseline_head" ]; then
		fail "$label: HEAD moved"
	elif [ "$(git status --porcelain)" != "$baseline_status" ]; then
		fail "$label: the working tree changed"
	else
		pass "$label: no edits and no commits"
	fi
}

# Check 1: headless Claude. stream-json needs --verbose. Assumed shape:
# {type:"assistant", message:{content:[{type:"tool_use", name, input}, ...]}}
# and a final {type:"result", result:"..."}. The project's .mcp.json is
# passed explicitly because -p does not prompt to approve project servers.
claude -p "fix the failing test" --output-format stream-json --verbose \
	--mcp-config .mcp.json >"$TMP/claude.jsonl" 2>"$TMP/claude.err" || true
jq -r 'select(.type=="assistant") | .message.content[]? | select(.type=="tool_use") | .name' \
	"$TMP/claude.jsonl" >"$TMP/claude.tools" 2>/dev/null || true
{
	jq -r 'select(.type=="assistant") | .message.content[]? | select(.type=="text") | .text' "$TMP/claude.jsonl"
	jq -r 'select(.type=="result") | .result // empty' "$TMP/claude.jsonl"
} >"$TMP/claude.text" 2>/dev/null || true
if [ ! -s "$TMP/claude.jsonl" ]; then
	fail "check 1 (claude): no output: $(head -c 300 "$TMP/claude.err")"
fi
verify_no_edit "check 1 (claude)" "$TMP/claude.tools" "$TMP/claude.text"
if jq -e 'select(.type=="assistant") | .message.content[]? | select(.type=="tool_use" and (.name=="Edit" or .name=="Write" or .name=="MultiEdit" or .name=="NotebookEdit"))' \
	"$TMP/claude.jsonl" >"$TMP/claude.edits" 2>/dev/null && [ -s "$TMP/claude.edits" ]; then
	fail "check 1 (claude): an edit tool was called"
fi

# Check 4, second half: the headless Claude run read .brigada/foreman.md.
jq -r 'select(.type=="assistant") | .message.content[]? | select(.type=="tool_use" and .name=="Read") | .input.file_path // empty' \
	"$TMP/claude.jsonl" >"$TMP/claude.reads" 2>/dev/null || true
if grep -Fq '.brigada/foreman.md' "$TMP/claude.reads"; then
	pass "check 4: claude read .brigada/foreman.md"
else
	fail "check 4: claude made no Read call on .brigada/foreman.md"
fi

# Check 2: codex exec --json, no overrides. Assumed shape (unverified): JSONL
# events carrying an .item whose .type is "mcp_tool_call" (with .server and
# .tool), "command_execution", "file_change" or "agent_message" (.text).
# plan_create is matched loosely by tool name in any mcp/tool item.
codex exec --json "fix the failing test" >"$TMP/codex.jsonl" 2>"$TMP/codex.err" || true
jq -r 'select(.item != null) | .item | select((.type // "") | test("mcp|tool"; "i")) | [.server, .tool, .name] | map(select(. != null)) | join("/")' \
	"$TMP/codex.jsonl" >"$TMP/codex.tools" 2>/dev/null || true
jq -r 'select(.item.type=="agent_message") | .item.text // empty' \
	"$TMP/codex.jsonl" >"$TMP/codex.text" 2>/dev/null || true
if [ ! -s "$TMP/codex.jsonl" ]; then
	fail "check 2 (codex): no output: $(head -c 300 "$TMP/codex.err")"
fi
verify_no_edit "check 2 (codex)" "$TMP/codex.tools" "$TMP/codex.text"
jq -r 'select(.item.type=="file_change") | .item.type' "$TMP/codex.jsonl" >"$TMP/codex.edits" 2>/dev/null || true
if [ -s "$TMP/codex.edits" ]; then
	fail "check 2 (codex): a file_change item appeared"
fi

# Check 3: the worker side. Assert the router reached origin main, submit
# directly, wait for the job, then read its transcript.
git --git-dir="$ORIGIN" show main:AGENTS.md >"$TMP/origin-agents.md"
if grep -Fq 'foreman.md' "$TMP/origin-agents.md"; then
	pass "check 3: router block is in AGENTS.md at origin main"
else
	fail "check 3: router block missing from AGENTS.md at origin main"
fi
echo "Fix the failing test TestAdd in this repository by fixing the code under test." >"$TMP/spec.md"
submit_out=$(brigada task submit --repo "$ORIGIN" --base main --spec "$TMP/spec.md" \
	--check "go test ./..." --title "release check: fix the failing test")
echo "$submit_out"
task_id=$(sed -n 's/^task \([0-9][0-9]*\) job.*/\1/p' <<<"$submit_out")
if [ -z "$task_id" ]; then
	fail "check 3: no task id in the submit output"
else
	# Poll the timeline for a terminal word (assumed; 20 minutes cap).
	finished=0
	for _ in $(seq 1 120); do
		brigada history --task "$task_id" >"$TMP/history.txt" 2>&1 || true
		if grep -Eiq 'done|succeeded|completed|failed|needs_help|cancel' "$TMP/history.txt"; then
			finished=1
			break
		fi
		sleep 10
	done
	brigada logs --task "$task_id" >"$TMP/worker.txt" 2>&1 || true
	cat "$TMP/history.txt" >>"$TMP/worker.txt"
	if [ "$finished" = 0 ]; then
		fail "check 3: the job did not finish in time"
	fi
	# Edits: the runner pushes a branch other than main with a non-test .go change.
	git --git-dir="$ORIGIN" for-each-ref --format='%(refname:short)' refs/heads >"$TMP/heads.txt"
	branch=$(awk '$0 != "main" {print; exit}' "$TMP/heads.txt")
	edited=0
	if [ -n "$branch" ]; then
		git --git-dir="$ORIGIN" diff --name-only "main..$branch" >"$TMP/changed.txt" || true
		if awk '/\.go$/ && !/_test\.go$/ {f=1} END {exit !f}' "$TMP/changed.txt"; then
			edited=1
		fi
	fi
	if [ "$edited" = 1 ]; then
		pass "check 3: the worker edited the code on $branch"
	else
		fail "check 3: no pushed branch with a non-test .go change"
	fi
	if grep -Eq 'mcp__brigada__|plan_create|plan_amend|ask_user|hold_acquire|hold_yield|decision_note' "$TMP/worker.txt"; then
		fail "check 3: the worker called a foreman tool"
	else
		pass "check 3: the worker called no foreman tool"
	fi
	if grep -Fq 'foreman.md' "$TMP/worker.txt"; then
		fail "check 3: the worker read .brigada/foreman.md"
	else
		pass "check 3: the worker did not read .brigada/foreman.md"
	fi
fi

# Check 4, first half: the rules are pointed to, not imported.
imports=$(git grep -nE '@[^ ]*foreman\.md' -- . || true)
if [ -n "$imports" ]; then
	fail "check 4: a file imports .brigada/foreman.md: $imports"
else
	pass "check 4: nothing imports .brigada/foreman.md"
fi

if [ "$FAILS" -gt 0 ]; then
	echo "$FAILS check(s) FAILED"
	exit 1
fi
echo "all checks passed"
