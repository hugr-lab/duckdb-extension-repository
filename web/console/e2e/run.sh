#!/usr/bin/env bash
# The console end to end (spec 0015): Keycloak in docker (two realms: "ops" for server administrators,
# "acme" for a tenant; passwords made for this run), kista serve on https with the console built and
# a tenant set up with the CLI, then Playwright (web/console/e2e). Needs docker, go, node (npm ci
# and npm run build done in web/console, Chromium installed by Playwright), python3, openssl.
set -euo pipefail
console="$(cd "$(dirname "$0")/.." && pwd)"
root="$(cd "$console/../.." && pwd)"
kc_port="${E2E_KC_PORT:-18790}"
kista_port="${E2E_KISTA_PORT:-18743}"
host_port="${E2E_HOST_PORT:-18444}"
# another repository a tenant adds as an upstream (spec 0012 phase 1d): its .well-known only
repo_port="${E2E_REPO_PORT:-18791}"
# the micro-frontend's test platform (mfe-host), on another origin than kista
host="http://127.0.0.1:$host_port"
kc="http://127.0.0.1:$kc_port"
# kista by a name the browser maps to loopback: egress refuses the deployment's own address, and
# Keycloak is on 127.0.0.1
kista="https://kista.test:$kista_port"
work="$(mktemp -d)"
name=kista-e2e-kc
pids=()
cleanup() {
	for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done
	[ -z "${E2E_KEEP:-}" ] && docker rm -f "$name" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# passwords for this run only, never printed
export E2E_PW_TOM E2E_PW_EVE E2E_PW_NINA E2E_PW_GINA E2E_PW_ROOT E2E_KC_ADMIN_PW
E2E_KC_ADMIN_PW="$(openssl rand -hex 12)"
E2E_PW_TOM="$(openssl rand -hex 12)"
E2E_PW_EVE="$(openssl rand -hex 12)"
E2E_PW_NINA="$(openssl rand -hex 12)"
E2E_PW_GINA="$(openssl rand -hex 12)"
E2E_PW_ROOT="$(openssl rand -hex 12)"

echo "e2e: Keycloak"
mkdir -p "$work/import"
KISTA="$kista" python3 - "$work/import" <<'PY'
import json, os, sys
kista = os.environ["KISTA"]
user = lambda n, pw, roles: {"username": n, "enabled": True, "email": f"{n}@example.test", "emailVerified": True,
    "firstName": n.title(), "lastName": "Test", "credentials": [{"type": "password", "value": pw, "temporary": False}],
    "realmRoles": roles + ["offline_access"]}  # the console asks for offline_access (a refresh token)
def client(cid, aud):
    # direct grants: the test platform's tokens (e2e/mfe.spec.ts) come from Playwright, with this run's passwords
    return {"clientId": cid, "publicClient": True, "standardFlowEnabled": True, "directAccessGrantsEnabled": True,
        "redirectUris": [f"{kista}/ui/*"], "webOrigins": [kista],
        # 70 s tokens: the console renews 10 s after a sign-in, which the session-ended test needs
        "attributes": {"pkce.code.challenge.method": "S256", "post.logout.redirect.uris": f"{kista}/ui/*", "access.token.lifespan": "70"},
        "protocolMappers": [{"name": "aud", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
            "config": {"included.custom.audience": aud, "access.token.claim": "true", "id.token.claim": "false"}}]}
e = os.environ
acme = {"realm": "acme", "enabled": True, "sslRequired": "none",
    "roles": {"realm": [{"name": "tenant-admin"}, {"name": "ext-admin"}, {"name": "platform-team"}]},
    "clients": [client("kista-console", f"{kista}/acme")],
    "users": [user("tom", e["E2E_PW_TOM"], ["tenant-admin"]), user("eve", e["E2E_PW_EVE"], ["ext-admin"]), user("nina", e["E2E_PW_NINA"], []),
        user("gina", e["E2E_PW_GINA"], ["platform-team"])]}
ops = {"realm": "ops", "enabled": True, "sslRequired": "none", "roles": {"realm": [{"name": "kista-admin"}]},
    "clients": [client("kista-server-console", "api://kista-server")], "users": [user("root", e["E2E_PW_ROOT"], ["kista-admin"])]}
for r in (acme, ops):
    json.dump(r, open(os.path.join(sys.argv[1], r["realm"] + ".json"), "w"))
PY
chmod -R a+rX "$work/import"
docker rm -f "$name" >/dev/null 2>&1 || true
docker run -d --name "$name" -p "127.0.0.1:$kc_port:8080" \
	-e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD="$E2E_KC_ADMIN_PW" \
	-v "$work/import:/opt/keycloak/data/import:ro" \
	quay.io/keycloak/keycloak:26.4@sha256:9409c59bdfb65dbffa20b11e6f18b8abb9281d480c7ca402f51ed3d5977e6007 start-dev --import-realm >/dev/null

echo "e2e: certificates and kista"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=kista.test" -addext "subjectAltName=DNS:kista.test" \
	-keyout "$work/key.pem" -out "$work/cert.pem" 2>/dev/null
mkdir -p "$work/keys"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/keys/a.pem" 2>/dev/null
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/keys/b.pem" 2>/dev/null  # added from the console (2b)
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/keys/att.pem" 2>/dev/null  # the feed's (spec 0011)
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/keys/s.pem" 2>/dev/null  # tenant scratch's (spec 0019)
# spec 0021: vault's build declares the licence vectors' test-only key; a licence for vault, 20 days
# long, to import
openssl pkey -in "$root/internal/license/testdata/test-only-2048.pem" -pubout -out "$work/lic.pub.pem" 2>/dev/null
(cd "$root" && GOWORK=off go build -o "$work/kista" ./cmd/kista && GOWORK=off go build -o "$work/fixture" ./web/console/e2e/fixture)
"$work/fixture" "$work/tresor.duckdb_extension" 1.0 a
"$work/fixture" "$work/hello.duckdb_extension" 1.0 b
"$work/fixture" "$work/vault.duckdb_extension" 1.0 c vault "$work/lic.pub.pem"
# spec 0035: vault 1.1 with the same key, a distributor's publisher's in the catalogue: another entry
"$work/fixture" "$work/vault11.duckdb_extension" 1.1 i vault "$work/lic.pub.pem"
# spec 0033 phase C: the catalogue's builds (below)
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/keys/n.pem" 2>/dev/null  # nightly's
"$work/fixture" "$work/geo09.duckdb_extension" 0.9 d geo_tools
"$work/fixture" "$work/geo10.duckdb_extension" 1.0 e geo_tools
"$work/fixture" "$work/hello2.duckdb_extension" 2.0 f hello_acme
"$work/fixture" "$work/maps10.duckdb_extension" 1.0 g maps
"$work/fixture" "$work/maps11.duckdb_extension" 1.1 h maps
# K4: a build a server administrator adds to the catalogue from the console, its footer read there
"$work/fixture" "$work/k4_tool.duckdb_extension" 0.4 j k4_tool
# spec 0036: geo_tools' icon (its distributor's publisher's), and one tom gives maps in the console
"$work/fixture" -icon "$work/geo.png" 64
"$work/fixture" -icon "$work/maps.png" 96
lic_key="$(openssl pkey -in "$root/internal/license/testdata/test-only-2048.pem" -pubout -outform DER 2>/dev/null | base64 | tr -d '\n')"
now=$(date +%s)
printf '{"v":1,"iss":"https://issuer.example","jti":"lic-e2e","sub":"acme","holder":"Acme Test","ext":"vault","plan":"pro","limits":{"max_attached_sources":5},"iat":%d,"exp":%d}' \
	"$now" "$((now + 20 * 86400))" >"$work/claims.json"
(cd "$root" && GOWORK=off go run ./internal/license/vectorgen sign -claims "$work/claims.json") >"$work/vault.kista-license"
# spec 0035: a licence for tresor, whose builds declare no key: no entry lists its key
printf '{"v":1,"iss":"https://issuer.example","jti":"lic-nokey","sub":"acme","ext":"tresor","plan":"pro","iat":%d,"exp":%d}' \
	"$now" "$((now + 20 * 86400))" >"$work/claims-nokey.json"
(cd "$root" && GOWORK=off go run ./internal/license/vectorgen sign -claims "$work/claims-nokey.json") >"$work/tresor.kista-license"
mkdir -p "$work/org/acme/.well-known"
printf '{"signature_keys":["%s"]}' "$lic_key" >"$work/org/acme/.well-known/duckdb-extension-repo.json"
repo_key="sha256:$(printf '%s' "$lic_key" | base64 -d | openssl dgst -sha256 | awk '{print $NF}')"
cat >"$work/kista.yaml" <<YAML
profile: dev
store: { kind: sqlite, sqlite: { path: $work/k.db } }
signers: { file_dir: $work/keys }
serve:
  public_url: $kista
  # every page of the suite comes from one address: a browser's bursts, not a client to throttle
  api_rate: 100
  api_burst: 200
  drain_timeout: 1ms  # stopped and started again below: no load balancer to drain
  listeners: [ { addr: 127.0.0.1:$kista_port, scheme: https, tls: { cert_file: $work/cert.pem, key_file: $work/key.pem } } ]
egress: { allow_loopback_http: true, allow: [ { cidr: 127.0.0.1/32, ports: [$kc_port, $repo_port] } ] }
auth:
  server_issuers:
    - { name: ops, url: $kc/realms/ops, required_claims: { azp: kista-server-console }, roles_claim: realm_access.roles }
  server_audiences: [ api://kista-server ]
  server_egress_allow: [ { cidr: 127.0.0.1/32, ports: [$kc_port] } ]
  server_admins: [ "role:ops|kista-admin" ]
gc: { interval: 24h }  # every replica runs this version: purging is on (spec 0016)
tenants: { min_suspended: 0s }  # spec 0019: deleted at once in the suite
bundles: { chunk_size: 1MiB }  # spec 0014: an upload of a few MiB in several chunks
ui:
  environment: e2e
  allowed_origins: [ $host ]
  server_clients: [ { issuer: ops, client_id: kista-server-console } ]
YAML
k() { "$work/kista" admin -config "$work/kista.yaml" "$@" >/dev/null; }
for i in $(seq 1 90); do curl -sf "$kc/realms/acme/.well-known/openid-configuration" >/dev/null && break; sleep 2; done
curl -sf "$kc/realms/ops/.well-known/openid-configuration" >/dev/null || { docker logs "$name" | tail -50; exit 1; }
k migrate
printf 'json\nparquet\n' >"$work/core.txt" # the version's core extensions (spec 0030)
k version add v2.0.0 -kind release -c-api v1.5.6 -core-extensions "$work/core.txt"
k tenant create acme -display-name Acme
k channel create acme/prod -kind signed
k channel versions acme/prod -add v2.0.0
k key add acme/prod -signer file:a.pem -active
k attestation-key add acme file:att.pem -active
k issuer add acme -name kc -url "$kc/realms/acme" -roles-claim realm_access.roles -require azp=kista-console
k issuer console acme kc -client-id kista-console
k grant add acme -principal "role:kc|tenant-admin" -verb admin
k grant add acme -principal "role:kc|ext-admin" -verb admin -channel prod -extension tresor
k extension-group add acme platform -description "What every hugr node installs"   # spec 0017
k extension-group members acme platform -add tresor
k grant add acme -principal "role:kc|platform-team" -verb admin -extension-group platform
k release add acme/prod "$work/tresor.duckdb_extension" -name tresor -unchecked
# spec 0021: a publisher a server administrator marked publishes vault (below), which records its
# licence key
k publisher add acme vault-ci
k grant add acme -principal publisher:vault-ci -verb publish -channel prod -extension vault
k publisher license-keys acme vault-ci on
# its key (spec 0023): the publication below is signed with it
ssh-keygen -q -t ed25519 -N '' -C vault-ci -f "$work/vault-ci"
k publisher key add acme vault-ci "$work/vault-ci.pub" -expires 1d
k publisher add acme plain-ci # spec 0037: the tenant's own publisher, its builds declaring no licence key
k release add acme/prod "$work/hello.duckdb_extension" -name hello_acme -unchecked
k tenant create scratch -display-name Scratch   # deleted from the console (spec 0019)
k channel create scratch/prod -kind signed
k key add scratch/prod -signer file:s.pem -active
# spec 0033 phase C: a distributor's publisher with a day's hold publishes geo_tools 0.9 and 1.0 and
# its own hello_acme into the catalogue (below); nightly follows it, which takes geo_tools and skips
# hello_acme (the tenant has it from its own source); maps 1.0 and 1.1 are the tenant's, placed
# nowhere
k channel create acme/nightly -kind signed
k channel versions acme/nightly -add v2.0.0
k key add acme/nightly -signer file:n.pem -active
# spec 0037: no minimum hold for distributors' publishers, so each one's own hold alone holds its
# builds (enterest-ci's day; vault-dist's none: its vault 1.1 is live at once)
k tenant distributor-hold acme 0
k publisher add acme enterest-ci -distributor enterest
ssh-keygen -q -t ed25519 -N '' -C enterest-ci -f "$work/enterest-ci"
k publisher key add acme enterest-ci "$work/enterest-ci.pub" -expires 1d
k publisher hold acme enterest-ci 24h
# spec 0035: a second distributor's publisher, without a hold, whose vault 1.1 (below) declares
# vault's key too
k publisher add acme vault-dist -distributor enterest
k publisher license-keys acme vault-dist on
ssh-keygen -q -t ed25519 -N '' -C vault-dist -f "$work/vault-dist"
k publisher key add acme vault-dist "$work/vault-dist.pub" -expires 1d
k channel follow acme/nightly publisher:enterest-ci
k catalogue add acme "$work/maps10.duckdb_extension" -name maps
k catalogue add acme "$work/maps11.duckdb_extension" -name maps
serve() {
	"$work/kista" serve -config "$work/kista.yaml" >>"$work/kista.log" 2>&1 &
	serve_pid=$!
	pids+=("$serve_pid")
	for i in $(seq 1 60); do curl -skf --resolve "kista.test:$kista_port:127.0.0.1" "$kista/api/v1/console" >/dev/null && break; sleep 1; done
	curl -skf --resolve "kista.test:$kista_port:127.0.0.1" "$kista/api/v1/console" >/dev/null || { cat "$work/kista.log"; exit 1; }
}
# signed <method> <content type> <publisher> <path> <file>: a request signed with the publisher's key
# (spec 0023)
signed() {
	local hash
	hash="$( { sha256sum 2>/dev/null || shasum -a 256; } < "$5" | cut -d' ' -f1)"
	printf 'kista-request v1\nmethod %s\npath %s\nsha256 %s\nsigned-at %s\n' "$1" "$4" "$hash" \
		"$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$work/statement"
	ssh-keygen -Y sign -q -f "$work/$3" -n kista-request@v1 <"$work/statement" >"$work/statement.sig"
	curl -skf --resolve "kista.test:$kista_port:127.0.0.1" -X "$1" -H "Content-Type: $2" \
		-H "Kista-Statement: $(base64 <"$work/statement" | tr -d '\r\n')" \
		-H "Kista-Signature: $(grep -v -- ----- "$work/statement.sig" | tr -d '\r\n')" \
		--data-binary "@$5" "$kista$4" >/dev/null \
		|| { echo "e2e: the signed $1 $4 failed"; tail -20 "$work/kista.log"; exit 1; }
}
# publish <publisher> <path> <file>: a publication
publish() { signed POST application/octet-stream "$@"; }
serve
# spec 0035 phase 2: vault-ci's vault public in prod, so a licence into its entry warns
publish vault-ci "/api/v1/tenants/acme/channels/prod/extensions/vault/releases?version=1.0&platform=linux_amd64&visibility=public" "$work/vault.duckdb_extension"
cat_path="/api/v1/tenants/acme/catalogue/extensions"
publish enterest-ci "$cat_path/geo_tools/builds?version=0.9&platform=linux_amd64" "$work/geo09.duckdb_extension"
publish enterest-ci "$cat_path/geo_tools/builds?version=1.0&platform=linux_amd64" "$work/geo10.duckdb_extension"
publish enterest-ci "$cat_path/hello_acme/builds?version=2.0&platform=linux_amd64" "$work/hello2.duckdb_extension"
publish vault-dist "$cat_path/vault/builds?version=1.1&platform=linux_amd64" "$work/vault11.duckdb_extension"
# spec 0036: the distributor's publisher sets geo_tools' icon and summary, signing the request
printf '{"summary":"Geometry tools for maps","icon":{"content_type":"image/png","data":"%s"}}' \
	"$(base64 <"$work/geo.png" | tr -d '\r\n')" >"$work/geo-meta.json"
signed PUT application/json enterest-ci "/api/v1/tenants/acme/catalogue/publisher:enterest-ci/extensions/geo_tools/meta" "$work/geo-meta.json"
# started again: the placement pass looks at every follow at its start (the skips), then every minute
kill "$serve_pid"
wait "$serve_pid" 2>/dev/null || true
# spec 0037: the distributor's licence in vault-dist's entry, a server administrator's import (the
# CLI): tom reads it, and writes it not
k license import acme "$work/vault.kista-license" -source publisher:vault-dist
serve

python3 -m http.server --bind 127.0.0.1 --directory "$work/org" "$repo_port" >"$work/repo.log" 2>&1 &
pids+=($!)
for i in $(seq 1 30); do curl -sf "http://127.0.0.1:$repo_port/acme/.well-known/duckdb-extension-repo.json" >/dev/null && break; sleep 1; done

echo "e2e: the test platform"
cd "$console"
HOST_PORT="$host_port" npx vite -c vite.host.config.ts >"$work/host.log" 2>&1 &
pids+=($!)
for i in $(seq 1 30); do curl -sf "$host/" >/dev/null && break; sleep 1; done
curl -sf "$host/" >/dev/null || { cat "$work/host.log"; exit 1; }

echo "e2e: Playwright"
E2E_EXT="$work/k4_tool.duckdb_extension" E2E_ICON="$work/maps.png" E2E_LICENSE="$work/vault.kista-license" E2E_LICENSE_NOKEY="$work/tresor.kista-license" E2E_REPO_URL="http://127.0.0.1:$repo_port/acme" E2E_REPO_KEY="$repo_key" E2E_HOST_URL="$host" E2E_KISTA_URL="$kista" E2E_KC_URL="$kc" E2E_KC_HOST="127.0.0.1:$kc_port" npx playwright test "$@" || { tail -50 "$work/kista.log"; exit 1; }
