#!/bin/sh
# land_ready gates a landing on CI for the remote brigada/landed head.
# BRIGADA_LAND_READY_GRACE_SECONDS sets the time allowed for a run to
# appear after the head commit's committer date (default 900 seconds).
#
# exit 0: the branch does not exist yet, or a run for its head succeeded.
# exit 1: the PR conflicts with its base, no run for the head appeared
#         within the grace period, a run for the head completed without
#         success, or a job still got no runner after max_attempts (3)
#         attempts. The first line gives the reason (or the failed run URL;
#         for a spent job the line "<jobs> got no runner after <n> attempts").
# exit 2: PR mergeability is unknown, no run for the head has appeared
#         within the grace period, a run or job is pending, a lookup fails,
#         or a failed run's only failures are jobs that got no runner (a
#         cancelled job with no step run): the script reruns those jobs and
#         waits, and when gh refuses the rerun it prints gh's error and
#         retries on the next call.
#
# Enable it with: brigada project set land_ready=scripts/land-ready.sh
set -u
branch=brigada/landed
grace=${BRIGADA_LAND_READY_GRACE_SECONDS:-900}
case $grace in *[!0-9]*|'') echo 'invalid BRIGADA_LAND_READY_GRACE_SECONDS'; exit 2;; esac

branch_info=$(gh api 'repos/{owner}/{repo}/branches/brigada%2Flanded' --jq '[.commit.sha, .commit.commit.committer.date] | join("|")' 2>&1) || {
  case $branch_info in
    *'HTTP 404'*)
      if gh api 'repos/{owner}/{repo}' --jq .name >/dev/null 2>&1; then exit 0; fi;;
  esac
  echo "gh branch lookup failed for $branch: $branch_info"
  exit 2
}
head=${branch_info%%|*}
commit_date=${branch_info#*|}
if [ -z "$head" ] || [ "$commit_date" = "$branch_info" ] || [ -z "$commit_date" ]; then
  echo "invalid branch lookup for $branch"
  exit 2
fi
pr=$(gh pr list --head "$branch" --state open --json number,mergeable,baseRefName \
  --jq '.[0] | if . == null then "none" else "\(.number)|\(.mergeable)|\(.baseRefName)" end') || {
  echo "gh pr list failed for $branch"
  exit 2
}
pr_state='no open PR'
if [ "$pr" != none ] && [ -n "$pr" ]; then
  number=${pr%%|*}
  rest=${pr#*|}
  mergeable=${rest%%|*}
  base=${rest#*|}
  pr_state="PR #$number $mergeable"
  case $mergeable in
    CONFLICTING) echo "PR #$number conflicts with $base: CI will not run"; exit 1;;
    UNKNOWN) echo "PR #$number mergeability is UNKNOWN"; exit 2;;
  esac
fi
run=$(gh run list --branch "$branch" --limit 100 --json status,conclusion,url,headSha,databaseId,attempt \
  --jq "(map(select(.headSha == \"$head\")) | .[0]) // .[0] | if . == null then \"none\" else \"\\(.headSha)|\\(.status)|\\(.conclusion // \"\")|\\(.databaseId)|\\(.attempt)|\\(.url)\" end") || {
  echo "gh run list failed for $branch"
  exit 2
}
run_head=${run%%|*}
if [ "$run" = none ] || [ -z "$run" ] || [ "$run_head" != "$head" ]; then
  timestamp=$(date -u -d "$commit_date" +%s 2>/dev/null) || timestamp=$(date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$commit_date" +%s 2>/dev/null) || {
    echo "cannot parse commit date for $head: $commit_date"
    exit 2
  }
  now=$(date -u +%s) || exit 2
  if [ "$((now - timestamp))" -ge "$grace" ]; then
    echo "no CI run for $head after ${grace}s: $pr_state"
    exit 1
  fi
  echo "no CI run for $head yet$(if [ "$run" != none ] && [ -n "$run" ]; then printf '; newest run is for %s' "$run_head"; fi)"
  exit 2
fi
rest=${run#*|}
status=${rest%%|*}
rest=${rest#*|}
conclusion=${rest%%|*}
rest=${rest#*|}
run_id=${rest%%|*}
rest=${rest#*|}
attempt=${rest%%|*}
url=${rest#*|}
if [ "$status" != completed ]; then
  echo "run for $head is $status: $url"
  exit 2
fi
if [ "$conclusion" = success ]; then
  exit 0
fi
if [ "$conclusion" = failure ]; then
  jobs=$(gh api "repos/{owner}/{repo}/actions/runs/$run_id/jobs?per_page=100" --jq '.jobs[] | "\(.name)|\(.status)|\(.conclusion // "")|\(if ((.steps // []) | map(select((.conclusion // "") != "" and .conclusion != "skipped")) | length) > 0 then "ran" else "none" end)"') || {
    echo "gh jobs lookup failed for run $run_id"
    exit 2
  }
  max_attempts=3
  total=0
  bad=0
  names=
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    total=$((total + 1))
    ran=${line##*|}
    line=${line%|*}
    job_conclusion=${line##*|}
    line=${line%|*}
    job_status=${line##*|}
    job_name=${line%|*}
    if [ "$job_status" != completed ]; then
      echo "job $job_name of run $run_id is $job_status: $url"
      exit 2
    fi
    if [ "$job_conclusion" = cancelled ] && [ "$ran" = none ]; then
      names="${names:+$names, }$job_name"
    elif [ "$job_conclusion" != success ]; then
      bad=1
    fi
  done <<EOF
$jobs
EOF
  if [ "$total" -gt 0 ] && [ "$bad" = 0 ] && [ -n "$names" ]; then
    case $attempt in *[!0-9]*|'') echo "invalid attempt for run $run_id: $attempt"; exit 2;; esac
    if [ "$attempt" -ge "$max_attempts" ]; then
      echo "$names got no runner after $attempt attempts"
      echo "$url"
      exit 1
    fi
    rerun=$(gh run rerun "$run_id" --failed 2>&1) || {
      echo "$rerun"
      exit 2
    }
    echo "$names got no runner on attempt $attempt of $max_attempts; rerun requested: $url"
    exit 2
  fi
fi
echo "$url"
echo "run for $head concluded $conclusion"
exit 1
