#!/usr/bin/env bash
# The image's smoke test (spec 0013): kista version, then kista serve as uid 65532 on a read-only
# root filesystem with a tmpfs at /var/lib/kista and the dev config beside this script, until
# /readyz answers on the probe listener.
#   deploy/docker/smoke.sh <image> [version]
set -euo pipefail

image=${1:?usage: smoke.sh <image> [version]}
want=${2:-}
here=$(cd "$(dirname "$0")" && pwd)
name=kista-smoke-$$
port=${KISTA_SMOKE_PORT:-58181}

run() {
	docker run --rm --user 65532:65532 --read-only --cap-drop ALL --security-opt no-new-privileges "$@"
}

line=$(run "$image" version)
echo "$line"
if [ -n "$want" ] && [[ "$line" != "kista $want ("* ]]; then
	echo "smoke: want version $want, got: $line" >&2
	exit 1
fi

cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT
run -d --name "$name" \
	--tmpfs /var/lib/kista:uid=65532,gid=65532,mode=0700 \
	-v "$here/smoke.yaml:/etc/kista/kista.yaml:ro" \
	-p "127.0.0.1:$port:8081" \
	"$image" >/dev/null

deadline=$((SECONDS + 60))
until body=$(curl -sf "http://127.0.0.1:$port/readyz"); do
	if ! docker ps -q -f "name=^${name}$" | grep -q . || ((SECONDS > deadline)); then
		echo "smoke: kista never became ready" >&2
		docker logs "$name" >&2 || true
		exit 1
	fi
	sleep 1
done
[ "$body" = ready ] || { echo "smoke: /readyz said: $body" >&2; exit 1; }
# the probe listener serves nothing else
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/.well-known/duckdb-extension-repo.json")
[ "$code" = 404 ] || { echo "smoke: the probe listener answered $code off its paths" >&2; exit 1; }
echo "smoke: ok"
