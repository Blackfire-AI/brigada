#!/usr/bin/env bash
# race-shard.sh runs one shard of CI's race job: go test -race -count=1
# over a slice of the module. Under -race internal/engine alone outlasts
# the rest of the module, so its tests are split by name across the
# engine shards; every other package runs whole in exactly one shard.
# The last shard takes every package no other shard names, so a new
# package is never left out, and the engine shards split whatever
# `go test -list` reports, so a new test is never left out either.
#
#   scripts/race-shard.sh <shard>    run the shard
#   scripts/race-shard.sh --list     print each shard's packages and engine tests
set -euo pipefail

engine_shards=3
shards="engine-1 engine-2 engine-3 core cmd rest"

mod="$(go list -m)"
engine="$mod/internal/engine"
core_re="^${mod}/internal/(store|runner|api)\$"
cmd_re="^${mod}/cmd/brigada\$"

# engine_tests prints the engine's top-level tests, fuzz targets and
# examples, sorted, one per line.
engine_tests() {
	CGO_ENABLED=1 go test -race -list . "$engine" | grep -E '^(Test|Fuzz|Example)' | LC_ALL=C sort
}

# engine_slice prints the tests of engine shard n (1-based): every
# engine_shards-th test of the sorted list, starting at the n-th.
engine_slice() {
	engine_tests | awk -v k="$engine_shards" -v n="$1" '(NR - 1) % k == n - 1'
}

# packages prints the packages of a non-engine shard.
packages() {
	case "$1" in
	core) go list ./... | grep -E "$core_re" ;;
	cmd) go list ./... | grep -E "$cmd_re" ;;
	rest) go list ./... | grep -vE "$core_re|$cmd_re|^${engine}\$" ;;
	esac
}

run() {
	local shard="$1"
	case "$shard" in
	engine-*)
		local n="${shard#engine-}"
		if ! [ "$n" -ge 1 ] 2>/dev/null || [ "$n" -gt "$engine_shards" ]; then
			echo "unknown shard $shard" >&2
			exit 1
		fi
		local tests
		tests="$(engine_slice "$n" | paste -sd'|' -)"
		if [ -z "$tests" ]; then
			echo "shard $shard has no tests" >&2
			exit 1
		fi
		echo "$engine: $(engine_slice "$n" | wc -l | tr -d ' ') tests"
		CGO_ENABLED=1 go test -race -count=1 -run "^(${tests})\$" "$engine"
		;;
	core | cmd | rest)
		local pkgs
		pkgs="$(packages "$shard")"
		if [ -z "$pkgs" ]; then
			echo "shard $shard has no packages" >&2
			exit 1
		fi
		echo "$pkgs"
		# shellcheck disable=SC2086
		CGO_ENABLED=1 go test -race -count=1 $pkgs
		;;
	*)
		echo "unknown shard $shard" >&2
		exit 1
		;;
	esac
}

if [ "${1:-}" = "--list" ]; then
	for shard in $shards; do
		case "$shard" in
		engine-*) engine_slice "${shard#engine-}" | awk -v p="$shard $engine" '{ print p, $0 }' ;;
		*) packages "$shard" | awk -v p="$shard" '{ print p, $0 }' ;;
		esac
	done
	exit 0
fi

run "${1:?usage: race-shard.sh <shard> | --list}"
