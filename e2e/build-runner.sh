#!/usr/bin/env bash
# build-runner.sh <out>: builds e2e/runner/runner.c against <out>/lib and <out>/include from
# build-duckdb.sh, with an rpath to <out>/lib. Cheap, so it is never cached.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
out=$(cd "${1:?usage: build-runner.sh <out>}" && pwd)
cc -O2 -Wall -o "$out/runner" "$here/runner/runner.c" -I"$out/include" \
	-L"$out/lib" -lduckdb -Wl,-rpath,"$out/lib"
