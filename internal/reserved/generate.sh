#!/usr/bin/env bash
# Regenerates DuckDB's extension aliases and signing keys (spec 0009) from DuckDB at e2e/DUCKDB_PIN.
# With a DuckDB checkout given as $1 (at the pin) it is used; otherwise the needed paths are fetched.
# The reserved names are the server's data (spec 0030): kista duckdb core-extensions.
set -euo pipefail
cd "$(dirname "$0")/../.."
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
duckdb_commit=$(sed -n 's/^DUCKDB_COMMIT=//p' e2e/DUCKDB_PIN)

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
		/src/main/extension/
fi
if [[ "$(git -C "$duckdb" rev-parse HEAD 2>/dev/null || true)" != "$duckdb_commit" ]]; then
	echo "generate.sh: $duckdb is not at $duckdb_commit" >&2
	exit 1
fi
GOWORK=off go run ./internal/reserved/gen -duckdb "$duckdb"
