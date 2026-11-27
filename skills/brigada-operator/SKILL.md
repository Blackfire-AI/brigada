---
name: brigada-operator
description: Operate brigada from this coding agent, taking a request from spec to landing through brigada's foreman. Writes the spec, delegates implementation with `brigada ask`, watches status, history, logs and diffs, verifies the result independently, closes follow-ups and lands. Use whenever the user asks to build, fix, continue, review, verify or land work through brigada — "ask brigada", "give it to the foreman", "hand it to brigada", "have brigada do it" — without naming the skill.
---

# Operating brigada from your coding agent

You are the operator: the user's own coding agent, outside brigada.
Brigada's foreman plans and its workers build; you pass the user's
requests to the foreman and report back through the brigada CLI.
Unless the user explicitly requests only one stage, you own the
whole sequence below, from the spec to the landing and the report.

## 1. Specify

Write the goal, the relevant context, the scope and non-goals,
numbered acceptance criteria and the required validation. Put the
spec in a pushed repository file or issue, or include its complete
text in the ask: a spec that exists only in chat is incomplete. The
foreman and its workers read what is pushed, not your working tree,
so push what the spec cites or put it in the ask.

## 2. Delegate implementation

Send the implementation request with `brigada ask -`, the message on
stdin, citing the spec and giving the foreman the repository
context, ordering and constraints it needs:

    brigada ask - <<'EOF'
    Fix the login timeout (issue #12). The issue says: <its text>.
    Relevant code: auth/session.go. Closes #12 in the commit.
    EOF

- The foreman reads the issues and pull requests of the project's
  own repository itself; a bare number of that repository is often
  enough. For an issue anywhere else, or when the repository is not
  on GitHub with no tracker/tracker_url set, or gh is not signed in, put its text into the ask; keep
  the number only as a reference for `Closes` or `Refs` trailers.
- Name files and document sections; the foreman reads the
  repository.
- Say the build order when tasks depend on each other.
- Hint a tier when a task is easy (light) or risky (heavy); the
  foreman decides.
- If an account is cooling (`brigada status` shows it), say so, or
  clear a fixed one with `brigada agent reset <harness>/<model>`.

The reply comes later: `brigada ask` returns at once, and each
headless foreman turn ends with a report, which `brigada status`
shows as `foreman report:`. Relay the report to the user, its
questions and estimates included; the foreman asks the user through
it. Send the user's answer or veto with another `brigada ask`,
naming the plan or task — it wakes the foreman. When a plan needs a
conversation, the user runs `brigada attach`; a message sent
meanwhile waits for them.

## 3. Watch

Follow status, history, logs and diffs. Poll in the background,
never in a foreground wait that blocks the session:

- `brigada status`: the engine, jobs, plans and what needs the
  user. Pipe it through grep for one plan or task;
  `brigada status --line` is one short line.
- `brigada history --task N`: one task's timeline.
- `brigada history --job N`: one job's timeline, its full verdict.
- `brigada logs --task N`: what task N's latest job did.
- `brigada diff --stat N`: what task N changed, as a diffstat.
- `brigada top` is the user's live view; do not run it.

After an outage (engine down, runner dead, a quota wall), run
`brigada start`, then `brigada ask` again: an ask re-wakes the
foreman, even when its wake budget for the day is spent.

## 4. Verify independently

Verification is its own stage after implementation, never part of
it. A task marked done, a passing worker check or a reviewer's
approval alone does not complete it.

- Map every acceptance criterion to concrete evidence.
- Inspect the complete resulting diff: `brigada diff --stat N` for
  the overview, then the full diff in the repository.
- Run the applicable project checks.
- Exercise the original user scenario end to end on the actual
  result.
- Review the check and reviewer evidence and investigate every
  unresolved finding.

## 5. Close follow-ups

Route every defect, omission, failed check and relevant review
finding back through brigada with another `brigada ask`. Watch the
resulting work (stage 3) and repeat the independent verification
(stage 4) until every acceptance criterion has evidence and no
known follow-up remains. Only then is the work complete.

## 6. Land and report

Follow the applicable landing, CI, pull-request merge and
engine/runner restart procedure:

- If the project's land_target is a staging branch and after_land
  opens a pull request, merge that pull request with a merge commit
  once its CI is green.
- If land_target is the default branch and after_land is none, the
  landing went straight to the default branch: there is no pull
  request to merge.
- After a brigada upgrade, or when the engine's or the runner's own
  code landed in the repository, and nothing pending needs the old
  behaviour, restart them: `brigada stop --engine`, then
  `brigada start`.

Finish with the spec reference, what landed, the verification
evidence, the follow-ups resolved and anything still waiting.

## Instructions and skills

Brigada puts the project's instructions into every prompt, one text
per role, and task skills into the prompts of the tasks that name
them. They are files in the repository:

- Role files: `.brigada/instructions/<role>.md` for foreman,
  plan_review, worker and reviewer.
- Skills: `.brigada/skills/<name>/SKILL.md`, in the directory
  project.yaml names as `skills_dir`.

Edit them like any file, then run `brigada project import` to apply
the change, and commit and push them too: import reads your working
copy, the foreman's checkout what is pushed.
`brigada instructions diff <role|skill>` compares a file with the
default it came from; `brigada init --take-default <role|skill>`
resets it.

To ask for a skill on a task, say so in the ask, naming the task:

    use skill bugfix on task 3
    no skills on task 4

Name only skills that exist under skills_dir. Say "no skills" when
a task should run on the instructions alone.

## The user's alone

These decisions stay the user's, and waiting for one is paused
work, not completed work. Never do them on your own initiative; say
what waits and let the user decide:

- Plan approvals: `brigada plan approve` and `brigada plan reject`.
- Land approvals: `brigada land approve`, `brigada land reject` and
  landing by hand with `brigada land`.
- Push approvals: `brigada push approve`.
- `--and-stop-asking` on any approval.
- Tags and releases.
- Rule changes: the approval limit, `brigada project set`,
  `brigada budget --wakes`, `.brigada/project.yaml`.
