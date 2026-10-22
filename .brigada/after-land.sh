#!/bin/sh
# after_land: the land job pushes approved work to the staging branch
# named by land_target (brigada/landed). This opens the pull request from
# that branch to main when none is open; later landings push onto the
# same branch and show up in the open pull request. Merge it with a merge
# commit so the staging branch stays an ancestor of main.
set -eu
head=brigada/landed
base=main
if [ -n "$(gh pr list --head "$head" --base "$base" --state open --json number --jq '.[].number')" ]; then
  exit 0
fi
git fetch -q origin "$base" "$head"
body=$(git log --format='- %s' "origin/$base..origin/$head")
gh pr create --base "$base" --head "$head" --title "Landed tasks" --body "Approved and landed by brigada:

$body"
