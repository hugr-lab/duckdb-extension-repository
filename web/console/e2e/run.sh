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
export E2E_PW_TOM E2E_PW_EVE E2E_PW_NINA E2E_PW_ROOT E2E_KC_ADMIN_PW
E2E_KC_ADMIN_PW="$(openssl rand -hex 12)"
E2E_PW_TOM="$(openssl rand -hex 12)"
E2E_PW_EVE="$(openssl rand -hex 12)"
E2E_PW_NINA="$(openssl rand -hex 12)"
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
    return {"clientId": cid, "publicClient": True, "standardFlowEnabled": True, "directAccessGrantsEnabled": False,
        "redirectUris": [f"{kista}/ui/*"], "webOrigins": [kista],
        # 70 s tokens: the console renews 10 s after a sign-in, which the session-ended test needs
        "attributes": {"pkce.code.challenge.method": "S256", "post.logout.redirect.uris": f"{kista}/ui/*", "access.token.lifespan": "70"},
        "protocolMappers": [{"name": "aud", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
            "config": {"included.custom.audience": aud, "access.token.claim": "true", "id.token.claim": "false"}}]}
e = os.environ
acme = {"realm": "acme", "enabled": True, "sslRequired": "none",
    "roles": {"realm": [{"name": "tenant-admin"}, {"name": "ext-admin"}]},
    "clients": [client("kista-console", f"{kista}/acme")],
    "users": [user("tom", e["E2E_PW_TOM"], ["tenant-admin"]), user("eve", e["E2E_PW_EVE"], ["ext-admin"]), user("nina", e["E2E_PW_NINA"], [])]}
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
(cd "$root" && GOWORK=off go build -o "$work/kista" ./cmd/kista && GOWORK=off go run ./web/console/e2e/fixture "$work/tresor.duckdb_extension" 1.0 a \
	&& GOWORK=off go run ./web/console/e2e/fixture "$work/hello.duckdb_extension" 1.0 b)
cat >"$work/kista.yaml" <<YAML
profile: dev
store: { kind: sqlite, sqlite: { path: $work/k.db } }
signers: { file_dir: $work/keys }
serve:
  public_url: $kista
  listeners: [ { addr: 127.0.0.1:$kista_port, scheme: https, tls: { cert_file: $work/cert.pem, key_file: $work/key.pem } } ]
egress: { allow_loopback_http: true, allow: [ { cidr: 127.0.0.1/32, ports: [$kc_port] } ] }
auth:
  server_issuers:
    - { name: ops, url: $kc/realms/ops, required_claims: { azp: kista-server-console }, roles_claim: realm_access.roles }
  server_audiences: [ api://kista-server ]
  server_egress_allow: [ { cidr: 127.0.0.1/32, ports: [$kc_port] } ]
  server_admins: [ "role:ops|kista-admin" ]
gc: { interval: 24h }  # every replica runs this version: purging is on (spec 0016)
ui:
  environment: e2e
  server_clients: [ { issuer: ops, client_id: kista-server-console } ]
YAML
k() { "$work/kista" admin -config "$work/kista.yaml" "$@" >/dev/null; }
for i in $(seq 1 90); do curl -sf "$kc/realms/acme/.well-known/openid-configuration" >/dev/null && break; sleep 2; done
curl -sf "$kc/realms/ops/.well-known/openid-configuration" >/dev/null || { docker logs "$name" | tail -50; exit 1; }
k migrate
k version add v2.0.0 -kind release -c-api v1.5.6
k tenant create acme -display-name Acme
k channel create acme/prod -kind signed
k channel versions acme/prod -add v2.0.0
k key add acme/prod -signer file:a.pem -active
k issuer add acme -name kc -url "$kc/realms/acme" -roles-claim realm_access.roles -require azp=kista-console
k issuer console acme kc -client-id kista-console
k grant add acme -principal "role:kc|tenant-admin" -verb admin
k grant add acme -principal "role:kc|ext-admin" -verb admin -channel prod -extension tresor
k release add acme/prod "$work/tresor.duckdb_extension" -name tresor -unchecked
k release add acme/prod "$work/hello.duckdb_extension" -name hello_acme -unchecked
"$work/kista" serve -config "$work/kista.yaml" >"$work/kista.log" 2>&1 &
pids+=($!)
for i in $(seq 1 60); do curl -skf --resolve "kista.test:$kista_port:127.0.0.1" "$kista/api/v1/console" >/dev/null && break; sleep 1; done
curl -skf --resolve "kista.test:$kista_port:127.0.0.1" "$kista/api/v1/console" >/dev/null || { cat "$work/kista.log"; exit 1; }

echo "e2e: Playwright"
cd "$console"
E2E_KISTA_URL="$kista" E2E_KC_URL="$kc" E2E_KC_HOST="127.0.0.1:$kc_port" npx playwright test "$@" || { tail -50 "$work/kista.log"; exit 1; }
