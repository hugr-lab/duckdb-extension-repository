#!/usr/bin/env bash
# build-duckdb.sh <out>: builds DuckDB at e2e/DUCKDB_PIN for the e2e harness.
#
# Produces in <out>:
#   lib/libduckdb.{so,dylib}  the library the runner links (build-runner.sh)
#   include/duckdb.h          its C API header
#   extensions/               loadable_extension_demo, demo_capi, httpfs (unsigned, as built)
# and, as working directories, src/ (the verified checkout) and build/ (the cmake tree). CI caches
# only lib/, include/ and extensions/.
#
# kista depends on DuckDB only: nothing here comes from another hugr-lab repository.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build-duckdb.sh <out>}
mkdir -p "$out"
out=$(cd "$out" && pwd)
# shellcheck source=/dev/null
source "$here/DUCKDB_PIN"

src="$out/src"
if [ ! -d "$src/.git" ]; then
	git init -q "$src"
	git -C "$src" remote add origin "${DUCKDB_GIT_URL:-https://github.com/duckdb/duckdb}"
fi
if [ "$(git -C "$src" rev-parse HEAD 2>/dev/null || true)" != "$DUCKDB_COMMIT" ]; then
	git -C "$src" fetch -q --depth 1 origin "$DUCKDB_COMMIT"
	git -C "$src" checkout -q --detach FETCH_HEAD
fi
head=$(git -C "$src" rev-parse HEAD)
if [ "$head" != "$DUCKDB_COMMIT" ]; then
	echo "checkout is $head, expected $DUCKDB_COMMIT" >&2
	exit 1
fi
if [ -n "$(git -C "$src" status --porcelain)" ]; then
	echo "checkout $src has local changes" >&2
	exit 1
fi
if ! grep -q "GIT_TAG $HTTPFS_COMMIT" "$src/.github/config/extensions/httpfs.cmake"; then
	echo "httpfs.cmake does not pin $HTTPFS_COMMIT" >&2
	exit 1
fi

openssl_root=""
if [ "$(uname)" = Darwin ] && command -v brew >/dev/null; then
	openssl_root=$(brew --prefix openssl@3)
fi

launcher=()
if command -v ccache >/dev/null; then
	launcher=(-DCMAKE_C_COMPILER_LAUNCHER=ccache -DCMAKE_CXX_COMPILER_LAUNCHER=ccache)
fi

# No version override: a shallow checkout gives v<release>.0-dev<n>, a dev version, so the extension
# folder and the extensions' metadata use the commit hash, as any dev build of this commit does.
build="$out/build"
cmake -S "$src" -B "$build" -G Ninja \
	-DCMAKE_BUILD_TYPE=Release \
	-DSTATICALLY_LINK_EXTENSIONS=core_functions \
	-DBUILD_EXTENSIONS=demo_capi \
	-DDUCKDB_EXTENSION_CONFIGS="$src/.github/config/extensions/httpfs.cmake" \
	-DBUILD_UNITTESTS=ON -DBUILD_SHELL=OFF \
	${openssl_root:+-DOPENSSL_ROOT_DIR="$openssl_root"} \
	${launcher[@]+"${launcher[@]}"}

ninja -C "$build" duckdb loadable_extension_demo_loadable_extension \
	demo_capi_loadable_extension httpfs_loadable_extension

mkdir -p "$out/extensions"
for name in loadable_extension_demo demo_capi httpfs; do
	file=$(find "$build" -name "$name.duckdb_extension" -type f | head -1)
	[ -n "$file" ] || { echo "$name.duckdb_extension not built" >&2; exit 1; }
	cp "$file" "$out/extensions/"
done

lib=$(find "$build/src" -maxdepth 1 \( -name 'libduckdb.so' -o -name 'libduckdb.dylib' \) | head -1)
[ -n "$lib" ] || { echo "libduckdb not built" >&2; exit 1; }
mkdir -p "$out/lib" "$out/include"
cp "$lib" "$out/lib/"
cp "$src/src/include/duckdb.h" "$out/include/"
echo "built DuckDB $DUCKDB_COMMIT into $out"
