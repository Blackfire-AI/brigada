#!/bin/sh
# land_ready: gates the landing on the CI state of the staging branch.
# Enable it with: brigada project set land_ready scripts/land-ready.sh
#
# Looks at the latest `gh run list` run on brigada/landed:
#   exit 0  the latest run succeeded, or the branch has no runs or does not exist
#   exit 1  the latest run failed; the first output line is its URL
#   exit 2  a run is in progress or queued, or gh itself failed
set -u
branch=brigada/landed
out=$(gh run list --branch "$branch" --limit 1 --json status,conclusion,url \
  --jq '.[] | [.status, .conclusion, .url] | @tsv' 2>&1) || {
  echo "gh run list failed: $out"
  exit 2
}
[ -n "$out" ] || exit 0
status=$(printf '%s\n' "$out" | cut -f1)
conclusion=$(printf '%s\n' "$out" | cut -f2)
url=$(printf '%s\n' "$out" | cut -f3)
if [ "$status" != completed ]; then
  echo "run $status: $url"
  exit 2
fi
if [ "$conclusion" = success ]; then
  exit 0
fi
echo "$url"
echo "latest run on $branch: $conclusion"
exit 1
