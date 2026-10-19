#!/usr/bin/env bash
# fetch-ci-tools.sh <out>: checks out hugr-lab's extension-ci-tools at e2e/CI_TOOLS_PIN into
# <out>/ci-tools, for the kista C++ SDK it holds (kista/sdk/cpp, spec 0022).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: fetch-ci-tools.sh <out>}
mkdir -p "$out"
out=$(cd "$out" && pwd)
# shellcheck source=/dev/null
source "$here/CI_TOOLS_PIN"

dir="$out/ci-tools"
if [ ! -d "$dir/.git" ]; then
	git init -q "$dir"
	git -C "$dir" remote add origin "${CI_TOOLS_GIT_URL:-$CI_TOOLS_REPO}"
fi
git -C "$dir" remote set-url origin "${CI_TOOLS_GIT_URL:-$CI_TOOLS_REPO}"
if [ "$(git -C "$dir" rev-parse HEAD 2>/dev/null || true)" != "$CI_TOOLS_COMMIT" ]; then
	# the pin must be a commit of the fork's main: GitHub serves a commit by hash from any fork of a
	# repository, so the hash alone does not say hugr-lab published it
	git -C "$dir" fetch -q --filter=blob:none origin main
	if ! git -C "$dir" merge-base --is-ancestor "$CI_TOOLS_COMMIT" origin/main; then
		echo "$CI_TOOLS_COMMIT is not a commit of extension-ci-tools' main" >&2
		exit 1
	fi
	git -C "$dir" checkout -q --detach "$CI_TOOLS_COMMIT"
fi
if [ "$(git -C "$dir" rev-parse HEAD)" != "$CI_TOOLS_COMMIT" ]; then
	echo "the extension-ci-tools checkout is not $CI_TOOLS_COMMIT" >&2
	exit 1
fi
if [ -n "$(git -C "$dir" status --porcelain)" ]; then
	echo "the extension-ci-tools checkout $dir has local changes" >&2
	exit 1
fi
