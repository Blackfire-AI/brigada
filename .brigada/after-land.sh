#!/bin/sh
# after_land: the land job pushes approved work to the staging branch
# named by land_target (brigada/landed). This opens the pull request from
# that branch to main, or rewrites its body when one is open, so the body
# always covers every landing on the branch: the commit subjects, then each
# commit's `Closes #N` line, which closes the issue when the pull request
# merges. Merge it with a merge commit so the staging branch stays an
# ancestor of main.
set -eu
head=brigada/landed
base=main
git fetch -q origin "$base" "$head"
range="origin/$base..origin/$head"
subjects=$(git log --format='- %s' "$range")
closes=$(git log --format=%B "$range" |
  sed -n -E 's/^[[:space:]]*([Cc]lose[sd]?|[Ff]ix(e[sd])?|[Rr]esolve[sd]?) #([0-9]+)[[:space:]]*$/Closes #\3/p' |
  sort -u)
body="Approved and landed by brigada:

$subjects"
if [ -n "$closes" ]; then
  body="$body

$closes"
fi
pr=$(gh pr list --head "$head" --base "$base" --state open --json number --jq '.[].number')
if [ -n "$pr" ]; then
  gh pr edit "$pr" --body "$body"
else
  gh pr create --base "$base" --head "$head" --title "Landed tasks" --body "$body"
fi
