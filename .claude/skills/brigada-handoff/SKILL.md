---
name: brigada-handoff
description: 'How a coding session in this repository hands work to the brigada foreman and follows it: spec on main first, `brigada ask` with pointers, watch status, report, close the issues that shipped. Use when the user says "hand to the foreman", "ask brigada", "plan this with brigada", "give it to brigada", or asks for brigada status or landing.'
---

# Handing work to the brigada foreman

This session is not the foreman. Planning, answering workers and restarting
tasks go through the brigada-owned foreman session: `brigada ask` for a
message, `brigada attach` for a conversation. Reference: `docs/GUIDE.md` §2
(attach, ask), §4 (while work runs), §5 (the land job), §7 (gates and rules).
Read a section there instead of guessing.

## 1. Before the ask: the spec lives in the repo

The foreman and every worker read their own checkout. A spec that exists only
in chat is lost to them.

- A design change: an issue first, then amend `docs/design.md` /
  `docs/product.md` on a branch, `brigada review --branch <name>` (another
  model family), fix, pull request to main.
- A bug or small feature: an issue number or a one-paragraph statement is
  enough; the foreman reads the code itself.

## 2. The message

```sh
brigada ask - <<'EOF'
<what to build, pointing at docs sections or issue numbers on main>
<build order where it matters; what is in scope and what is not>
<tier hints: light for mechanical edits, heavy for engine/store rules>
<capacity notes: which accounts are cooling>
EOF
```

Each task that works on a GitHub issue says in the brief "commit body ends
with `Closes #N`" (the whole issue) or "`Refs #N`" (part of it), as AGENTS.md
asks. The commit message is what closes the issue when the landed pull
request merges; a `#N` in a task title or `(issue #N)` in prose closes
nothing.

Pointers, not prose copied from the docs. One ask per project. `brigada
attach` only when the plan needs discussion; detach writes the session back.

## 3. Watch, and hand the user only decisions

- `brigada status` (filter out `done`/`cancelled` lines), `brigada history
  --task N`, `brigada logs --task N`, `brigada diff N --stat`.
- A plan held by a rule waits in the needs-you queue: `brigada plan approve
  N`, `brigada land approve <task>` and `brigada push approve` are the user's
  commands. Report the plan table and the foreman's estimate, then stop.
  Never approve for them.
- Poll in the background (exit on `pending_approval`, `needs_user` or a held
  gate), report once. No foreground sleeps.
- `needs_foreman` is the foreman's; `needs_user` and a held gate are the
  user's. Everything else is a report, not a question.

## 4. Landing

The land job squashes each approved task onto `land_target`
(`brigada/landed`), and `.brigada/after-land.sh` opens or refreshes the pull
request to main, repeating each commit's `Closes #N` line in its body. Watch
the land and push gates in the needs-you queue. The pull request is merged
with a merge commit, so the staging branch stays an ancestor of main.

## 5. Report

What landed (task → commit), what runs, what waits at a gate, what needs
the user. Nothing mechanical handed back ("run X" is wrong when X can be run
here).

After a landed pull request merges, and before any release tag, list the
issues CHANGELOG.md names that are still open, and close each fully shipped
one with its commit (`gh issue close N -c "Landed in <sha>."`); leave partial
ones open. A `#N` the changelog uses as an example is not an issue to close.

```sh
grep -o '#[0-9]\+' CHANGELOG.md | sort -u | tr -d '#' | while read n; do
  [ "$(gh issue view "$n" --json state -q .state 2>/dev/null)" = OPEN ] && echo "#$n open"
done
```
