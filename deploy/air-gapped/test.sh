#!/usr/bin/env bash
# Runs the shell blocks of website/docs/deployment/air-gapped.md, in order (spec 0014): the blocks
# marked ci=connected in a container with a network, those marked ci=site in a container on an
# internal Docker network whose only other member is the site's registry (TLS, its own CA). The
# yaml block marked ci=site-config is appended to the site's kista.yaml, with the fingerprint the
# keygen printed. Against the last release; before the first one, against :edge (signed by
# image.yml), without the blocks marked ci=*-release (there is no chart or binary yet).
#
# Needs docker, go, python3, openssl, and linux binaries of cosign, oras and gh for the containers'
# architecture in $TOOLS (default: the ones on the PATH). GH_TOKEN for gh on the connected side.
# The kista that runs the cycle on both sides is built from this checkout, so the page is tested
# against the code beside it; the image, the chart and the binary verified are the release's.
#   deploy/air-gapped/test.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
page=website/docs/deployment/air-gapped.md
repo=hugr-lab/duckdb-extension-repository
base=debian:stable-slim
id=$$
net=kista-airgap-$id
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/kista-airgap.XXXXXX")
cleanup() {
	docker rm -f "kista-airgap-registry-$id" "kista-airgap-site-$id" "kista-airgap-connected-$id" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# The version: the last release, or :edge (a failing gh fails the test, not falls back)
version=$(gh release list -R "$repo" -L 1 --exclude-drafts --exclude-pre-releases --json tagName -q '.[0].tagName // ""')
if [ -n "$version" ]; then
	workflow=$repo/.github/workflows/release.yml
	identity=https://github.com/$workflow@refs/tags/$version
	release=1
else
	version=edge
	workflow=$repo/.github/workflows/image.yml
	identity=https://github.com/$workflow@refs/heads/main
	release=
	echo "no release yet: testing :edge, without the chart and binary blocks"
fi
arch=$(docker info --format '{{.Architecture}}')
case "$arch" in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; esac

# The tools, kista, and the CA certificates from the image
mkdir -p "$work/tools" "$work/media" "$work/connected/etc" "$work/connected/home" "$work/site/etc" \
	"$work/site/home" "$work/site/ca"
for t in cosign oras gh; do
	src=${TOOLS:+$TOOLS/$t}
	cp "${src:-$(command -v "$t")}" "$work/tools/$t"
done
image=ghcr.io/hugr-lab/kista:$version
docker pull -q --platform "linux/$arch" "$image" >/dev/null
c=$(docker create --platform "linux/$arch" "$image")
docker cp -q "$c:/etc/ssl/certs/ca-certificates.crt" "$work/connected/ca-certificates.crt"
docker rm "$c" >/dev/null
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$work/tools/kista" ./cmd/kista

# The configurations: what each side needs beyond the page (store, signers, blobs)
for side in connected site; do
	mkdir -p "$work/$side/keys" "$work/$side/data"
	chmod 700 "$work/$side/keys"
	openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/$side/keys/signer.pem" 2>/dev/null
	chmod 600 "$work/$side/keys/signer.pem"
	cat >"$work/$side/etc/kista.yaml" <<EOF
store:
  kind: sqlite
  sqlite:
    path: /var/lib/kista/data/kista.db
signers:
  allow_file: true
  file_dir: /var/lib/kista/keys
blob:
  domains:
    - name: default
      kind: fs
      fs:
        root: /var/lib/kista/data/blobs
  spool_dir: /var/lib/kista/data/spool
EOF
done

# The site's registry, with its own CA
registry=registry.site.internal
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=site-ca -keyout "$work/site/ca/ca.key" \
	-out "$work/site/ca/ca.pem" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=$registry" -keyout "$work/site/ca/tls.key" \
	-out "$work/site/ca/tls.csr" 2>/dev/null
printf 'subjectAltName=DNS:%s\n' "$registry" >"$work/site/ca/ext"
openssl x509 -req -in "$work/site/ca/tls.csr" -CA "$work/site/ca/ca.pem" -CAkey "$work/site/ca/ca.key" \
	-CAcreateserial -days 1 -extfile "$work/site/ca/ext" -out "$work/site/ca/tls.crt" 2>/dev/null
chmod 644 "$work/site/ca/tls.key"
docker network create --internal "$net" >/dev/null
docker run -d --name "kista-airgap-registry-$id" --network "$net" --network-alias "$registry" \
	-v "$work/site/ca:/certs:ro" -e REGISTRY_HTTP_ADDR=0.0.0.0:443 \
	-e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt -e REGISTRY_HTTP_TLS_KEY=/certs/tls.key \
	registry:2 >/dev/null

# The variables the page sets
cat >"$work/env" <<EOF
VERSION=$version
WORKFLOW=$workflow
IDENTITY=$identity
SITE_REGISTRY=$registry
PLATFORM=linux_amd64
DUCKDB_VERSION=v1.4.1
GATEWAY_SIGNER=file:signer.pem
SITE_SIGNER=file:signer.pem
PATH=/opt/tools:/usr/local/bin:/usr/bin:/bin
HOME=/home/kista
EOF
user=$(id -u):$(id -g)
start() { # name, network, ca file, side, docker run flags...
	docker run -d --name "kista-airgap-$1-$id" --network "$2" --user "$user" --env-file "$work/env" \
		-e SSL_CERT_FILE=/etc/kista-ca.pem "${@:5}" \
		-v "$3:/etc/kista-ca.pem:ro" -v "$work/tools:/opt/tools:ro" -v "$work/media:/work" \
		-v "$work/$4/etc:/etc/kista:ro" -v "$work/$4/keys:/var/lib/kista/keys:ro" \
		-v "$work/$4/data:/var/lib/kista/data" -v "$work/$4/home:/home/kista" \
		-w /work "$base" sleep infinity >/dev/null
}
docker pull -q "$base" >/dev/null
start connected bridge "$work/connected/ca-certificates.crt" connected ${GH_TOKEN:+-e GH_TOKEN}
start site "$net" "$work/site/ca/ca.pem" site
if docker exec "kista-airgap-site-$id" timeout 5 bash -c '</dev/tcp/ghcr.io/443' 2>/dev/null; then
	echo "FAIL the site reaches ghcr.io"
	exit 1
fi

# The page's blocks, in order
prelude='ka() { kista admin -config /etc/kista/kista.yaml "$@"; }'
python3 - "$page" "$work/blocks" <<'EOF'
import os, re, sys
page, out = sys.argv[1], sys.argv[2]
os.makedirs(out)
n, block, lines = 0, None, []
for line in open(page):
    s = line.strip()
    if block is None:
        m = re.match(r"^```(\w+)\s.*\bci=([a-z-]+)", s)
        if m:
            block, lines = m.group(2), []
    elif s == "```":
        n += 1
        # a block in a list item is indented
        ind = min((len(l) - len(l.lstrip()) for l in lines if l.strip()), default=0)
        with open(f"{out}/{n:02d}-{block}", "w") as f:
            f.write("".join(l[ind:] for l in lines))
        block = None
    else:
        lines.append(line)
EOF
for f in "$work"/blocks/*; do
	kind=${f##*/??-}
	case "$kind" in
	*-release) [ -n "$release" ] || { echo "skip ${f##*/}"; continue; } ;;
	esac
	echo "== ${f##*/}"
	case "$kind" in
	connected | connected-release | site | site-release)
		side=${kind%-release}
		docker exec -i "kista-airgap-$side-$id" bash -euo pipefail -c "$prelude
$(cat "$f")"
		;;
	site-config)
		fp=$(cat "$work/media/transfer/bundle-key.fingerprint")
		[[ "$fp" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "FAIL the fingerprint: $fp"; exit 1; }
		sed "s/sha256:…/$fp/" "$f" >>"$work/site/etc/kista.yaml"
		;;
	*)
		echo "FAIL unknown block ci=$kind"
		exit 1
		;;
	esac
done

# What the site holds
docker exec "kista-airgap-site-$id" bash -euo pipefail -c "$prelude
ka release list acme/core
[ \"\$(ka release list acme/core | grep -c ' active ')\" = 2 ]" ||
	{ echo "FAIL the site holds no release of json and httpfs"; exit 1; }
echo "air-gapped: ok ($version)"
