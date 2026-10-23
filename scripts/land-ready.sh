#!/bin/sh
# land_ready: the gate a landing passes before the land job squashes
# anything onto land_target (brigada/landed) and again before it pushes.
# It reads the newest workflow run on that staging branch, so a landing
# waits while the previous push is still being built and stops when the
# build is red.
#
#   exit 0  the latest run succeeded, or the branch has no runs / does
#           not exist: the landing proceeds
#   exit 1  the latest run finished without success (failure, cancelled,
#           skipped, timed out, ...): the landing is held, and the first
#           output line is the run's URL
#   exit 2  the latest run is queued or in progress, or gh itself failed:
#           not ready, the landing is retried on the backoff
#
# Enable it with: brigada project set land_ready scripts/land-ready.sh
# The file must be executable: chmod +x scripts/land-ready.sh
set -u
branch=brigada/landed
run=$(gh run list --branch "$branch" --limit 1 --json status,conclusion,url \
  --jq '.[0] | if . == null then "none" else "\(.status)|\(.conclusion // "")|\(.url)" end') || {
  echo "gh run list failed for $branch"
  exit 2
}
if [ "$run" = none ] || [ -z "$run" ]; then
  exit 0
fi
status=${run%%|*}
rest=${run#*|}
conclusion=${rest%%|*}
url=${rest#*|}
if [ "$status" != completed ]; then
  echo "latest run on $branch is $status: $url"
  exit 2
fi
case $conclusion in
success)
  exit 0
  ;;
*)
  echo "$url"
  echo "latest run on $branch concluded $conclusion"
  exit 1
  ;;
esac
