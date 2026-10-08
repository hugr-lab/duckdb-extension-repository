#!/usr/bin/env bash
# Regenerates internal/reserved's lists (spec 0008) from DuckDB at e2e/DUCKDB_PIN and
# duckdb/community-extensions at internal/reserved/COMMUNITY_PIN. With a DuckDB checkout given as
# $1 (at the pin) it is used; otherwise the needed paths are fetched. The lists only grow.
set -euo pipefail
cd "$(dirname "$0")/../.."
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
duckdb_commit=$(sed -n 's/^DUCKDB_COMMIT=//p' e2e/DUCKDB_PIN)
community_commit=$(grep -v '^#' internal/reserved/COMMUNITY_PIN | tr -d '[:space:]')

fetch() { # repo commit dir paths...
	local repo=$1 commit=$2 dir=$3
	shift 3
	git init -q "$dir"
	git -C "$dir" remote add origin "$repo"
	git -C "$dir" sparse-checkout set --no-cone "$@"
	git -C "$dir" fetch -q --depth 1 --filter=blob:none origin "$commit"
	git -C "$dir" checkout -q FETCH_HEAD
}

duckdb=${1:-}
if [[ -z "$duckdb" ]]; then
	duckdb="$work/duckdb"
	fetch https://github.com/duckdb/duckdb "$duckdb_commit" "$duckdb" \
		/src/main/extension/ /src/include/duckdb/main/extension_entries.hpp /extension/ /.github/config/
fi
if [[ "$(git -C "$duckdb" rev-parse HEAD 2>/dev/null || true)" != "$duckdb_commit" ]]; then
	echo "generate.sh: $duckdb is not at $duckdb_commit" >&2
	exit 1
fi
fetch https://github.com/duckdb/community-extensions "$community_commit" "$work/community" /extensions/
GOWORK=off go run ./internal/reserved/gen -duckdb "$duckdb" -community "$work/community"
