#!/usr/bin/env bash
# Azure, live (spec 0020 phase 3): the Container Apps recipe (deploy/azure-container-apps) applied
# whole in a resource group of its own: kista's managed identity the only credential, signing with
# a Key Vault HSM key, bodies in Blob Storage, the store on PostgreSQL Flexible Server (or Azure SQL)
# by Entra authentication over verify-full. Administration is Entra's: two app registrations, one
# whose tokens are kista's server audience, one for tenant acme's, and the first as the client
# (client credentials). `check` runs the kind test's checks through Container Apps' ingress.
#
#   deploy/azure/recipe-live.sh up | check | down | leftovers
#
# `down` deletes the group, purges the vault, deletes the custom role and the app registrations;
# `leftovers` lists whatever of the run's remains. The run's state is in deploy/azure/.state-live
# (the client secret among it, mode 0600; it expires in two days and goes with its app).
#
# Optional: KISTA_AZURE_LOCATION (westeurope), KISTA_AZURE_DATABASE (postgres or sqlserver),
# KISTA_LIVE_IMAGE (ghcr.io/hugr-lab/kista:edge, pinned by its digest at up). The subscription is
# az's current one; the signed-in account needs Owner (the recipe makes a role definition) and may
# create app registrations. Needs az with bicep, jq, curl, go; the DuckDB e2e build
# (make e2e-build) for check.
set -euo pipefail

cmd=${1:?usage: recipe-live.sh up | check | down | leftovers}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
export GOWORK=off
location=${KISTA_AZURE_LOCATION:-westeurope}
database=${KISTA_AZURE_DATABASE:-postgres}
name=kista-live
work=$here/.state-live
mkdir -p "$work" && chmod 700 "$work"

step() { printf '\n== %s\n' "$*"; }
save() { echo "$1=$2" >>"$work/state"; }
get() { [ -f "$work/state" ] && sed -n "s/^$1=//p" "$work/state" | tail -1 || true; }
out() { az deployment group show -g "$(get group)" -n kista --query "properties.outputs.$1.value" -o tsv; }
# the claims of a JWT's payload
claims() { local p=${1#*.}; p=${p%%.*}; p=$(printf '%s' "$p" | tr '_-' '/+'); while [ $((${#p} % 4)) != 0 ]; do p="$p="; done; printf '%s' "$p" | base64 -d; }

# an Entra app registration: its app id; made once, with its service principal
app() {
	local display=$1 id
	id=$(az ad app list --display-name "$display" --query '[0].appId' -o tsv)
	if [ -z "$id" ]; then
		id=$(az ad app create --display-name "$display" --identifier-uris "api://$display" --query appId -o tsv)
		# v2.0 tokens: the issuer https://login.microsoftonline.com/<tenant>/v2.0
		az ad app update --id "$id" --set api.requestedAccessTokenVersion=2
		az ad sp create --id "$id" >/dev/null
	fi
	echo "$id"
}

# a client-credentials token of the client app for a resource app
token() {
	curl -sf "https://login.microsoftonline.com/$(get tenant)/oauth2/v2.0/token" \
		--data-urlencode grant_type=client_credentials --data-urlencode "client_id=$(get client)" \
		--data-urlencode "client_secret@$work/secret" --data-urlencode "scope=api://$1/.default" | jq -r .access_token
}

up() {
	group=$(get group)
	if [ -z "$group" ]; then
		group=$name-$(date +%m%d)-$RANDOM
		save group "$group"
	fi
	tenant=$(az account show --query tenantId -o tsv)
	save tenant "$tenant"

	step "Entra: the server's and tenant acme's app registrations, and the client's secret"
	server=$(app "$group-server")
	acme=$(app "$group-acme")
	save client "$server"
	save server_app "$group-server"
	save acme_app "$group-acme"
	if [ ! -s "$work/secret" ]; then
		(umask 077; az ad app credential reset --id "$server" --display-name live --end-date "$(date -u -v+2d +%Y-%m-%d 2>/dev/null || date -u -d +2days +%Y-%m-%d)" \
			--query password -o tsv | tr -d '\n' >"$work/secret")
	fi
	# a new secret and a new principal take a while to work
	for i in $(seq 1 30); do
		t=$(token "$group-server" 2>/dev/null || true)
		[ -n "$t" ] && [ "$t" != null ] && break
		[ "$i" = 30 ] && { echo "up: no token for api://$group-server" >&2; exit 1; }
		sleep 10
	done
	claims "$t" | jq '{iss, aud, sub, azp, ver}'
	iss=$(claims "$t" | jq -r .iss) aud=$(claims "$t" | jq -r .aud) sub=$(claims "$t" | jq -r .sub)
	save iss "$iss"
	save sub "$sub"
	save server_aud "$aud"

	image=$(get image)
	if [ -z "$image" ]; then
		ref=${KISTA_LIVE_IMAGE:-ghcr.io/hugr-lab/kista:edge}
		if [[ $ref != *@sha256:* ]]; then
			repo_path=${ref#ghcr.io/} tagname=${ref##*:}
			repo_path=${repo_path%:*}
			ghcr=$(curl -sf "https://ghcr.io/token?scope=repository:$repo_path:pull" | jq -r .token)
			digest=$(curl -sfI -H "Authorization: Bearer $ghcr" -H 'Accept: application/vnd.oci.image.index.v1+json' \
				"https://ghcr.io/v2/$repo_path/manifests/$tagname" | tr -d '\r' | sed -n 's/^docker-content-digest: //Ip')
			[ -n "$digest" ] || { echo "up: no digest for $ref" >&2; exit 1; }
			ref=${ref%:*}@$digest
		fi
		image=$ref
		save image "$image"
	fi
	echo "image: $image"

	step "the resource group $group ($location)"
	az group create -n "$group" -l "$location" --tags "purpose=$name" -o none

	step "the Container Apps recipe (deploy/azure-container-apps), $database; about 20 minutes"
	jq -n --arg image "$image" --arg iss "$iss" --arg azp "$server" --arg aud "$aud" --arg sub "$sub" --arg db "$database" '{
		"$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
		contentVersion: "1.0.0.0",
		parameters: {
			prefix: {value: "kista-live"},
			image: {value: $image},
			database: {value: $db},
			serverIssuers: {value: [{name: "entra", url: $iss, required_claims: {azp: $azp}}]},
			serverAudiences: {value: [$aud]},
			serverAdmins: {value: ["subject:entra|\($sub)"]},
			purgeProtection: {value: false},
			softDeleteDays: {value: 7},
			tags: {value: {purpose: "kista-live"}}
		}}' >"$work/parameters.json"
	az deployment group create -g "$group" -n kista -f "$repo/deploy/azure-container-apps/main.bicep" \
		-p @"$work/parameters.json" -o none
	echo "up: kista at $(out url)"
}

api() {
	local method=$1 path=$2 tok=$3
	shift 3
	curl -sS -X "$method" -H "Authorization: Bearer $tok" "$@" "$(out url)/api/v1/$path"
}

# a 2xx, or a 409 (it exists: check runs again)
ok() {
	local code
	code=$(api "$@" -o "$work/body" -w '%{http_code}')
	case "$code" in
	2?? | 409) ;;
	*) echo "$1 $2: $code $(cat "$work/body")" >&2; exit 1 ;;
	esac
}

check() {
	group=$(get group)
	url=$(out url)
	step "$url answers over http and https"
	for scheme in http https; do
		for i in $(seq 1 60); do
			[ "$(curl -s -o /dev/null -w '%{http_code}' "$scheme://${url#https://}/readyz")" = 200 ] && break
			[ "$i" = 60 ] && { echo "$scheme://${url#https://}/readyz never answered 200" >&2; exit 1; }
			sleep 5
		done
	done

	step "the first administration through the API: the server administrator is an Entra app"
	srv=$(token "$(get server_app)")
	ten=$(token "$(get acme_app)")
	acme_aud=$(claims "$ten" | jq -r .aud)
	api GET whoami "$srv" -f | jq -e '.administrator == true' >/dev/null
	key=$(out channelSigner)
	duck=$repo/e2e/.build
	go build -o "$work/kista" "$repo/cmd/kista"
	field() { "$work/kista" ext inspect "$1" | awk -F': *' -v k="$2" '$1 == k {print $2}'; }
	version=$(field "$duck/extensions/httpfs.duckdb_extension" "duckdb version")
	case "$version" in v*) vkind=release ;; *) vkind=dev ;; esac
	json='Content-Type: application/json'
	principal="subject:entra|$(get sub)"
	ok POST tenants "$srv" -H "$json" -d '{"name":"acme"}'
	ok POST tenants/acme/audiences "$srv" -H "$json" -d "$(jq -cn --arg a "$acme_aud" '{audience: $a}')"
	ok POST tenants/acme/channels "$srv" -H "$json" -d '{"name":"prod","kind":"signed"}'
	ok POST duckdb-versions "$srv" -H "$json" -d "$(jq -cn --arg n "$version" --arg k "$vkind" '{name: $n, kind: $k}')"
	ok POST tenants/acme/channels/prod/duckdb-versions "$srv" -H "$json" -d "$(jq -cn --arg v "$version" '{duckdb_version: $v}')"
	ok POST tenants/acme/channels/prod/keys "$srv" -H "$json" -d "$(jq -cn --arg s "$key" '{signer_ref: $s, active: true}')"
	ok POST tenants/acme/issuers "$srv" -H "$json" -d "$(jq -cn --arg u "$(get iss)" '{name: "entra", url: $u}')"
	for v in publish install; do
		ok POST tenants/acme/grants "$srv" -H "$json" -d "$(jq -cn --arg p "$principal" --arg v "$v" '{principal: $p, verbs: [$v], channel: "prod"}')"
	done
	# httpfs is DuckDB's (a reserved name): publishing it needs a grant that names it
	ok POST tenants/acme/grants "$srv" -H "$json" -d "$(jq -cn --arg p "$principal" \
		'{principal: $p, verbs: ["publish"], channel: "prod", extension: "httpfs"}')"

	step "publication through the ingress: signed by Key Vault, stored in Blob Storage"
	for ext in httpfs:public loadable_extension_demo:private; do
		f=$duck/extensions/${ext%%:*}.duckdb_extension
		ok POST "tenants/acme/channels/prod/extensions/${ext%%:*}/releases?version=$(field "$f" "extension version")&platform=$(field "$f" platform)&visibility=${ext##*:}" \
			"$ten" -H 'Content-Type: application/octet-stream' --data-binary "@$f"
	done
	curl -sf "$url/acme/prod/.well-known/duckdb-extension-repo.json" >"$work/wellknown.json"
	jq -r '.signature_keys[0]' "$work/wellknown.json" >"$work/channel.pem"
	grep -q "BEGIN PUBLIC KEY" "$work/channel.pem"

	step "an admin command as a job: blob check"
	execution=$(az containerapp job start -g "$group" -n kista-live-blob-check --query name -o tsv)
	for i in $(seq 1 60); do
		status=$(az containerapp job execution show -g "$group" -n kista-live-blob-check --job-execution-name "$execution" --query properties.status -o tsv)
		case "$status" in Succeeded) break ;; Failed | Stopped | Degraded) echo "blob check: $status" >&2; exit 1 ;; esac
		[ "$i" = 60 ] && { echo "blob check: still $status" >&2; exit 1; }
		sleep 10
	done
	echo "blob check: $status"

	step "DuckDB through the ingress"
	KISTA_E2E_DUCKDB=$duck KISTA_E2E_URL=$url KISTA_E2E_CHANNEL=acme/prod KISTA_E2E_CA=/etc/ssl/cert.pem \
		KISTA_E2E_KEY="$work/channel.pem" KISTA_E2E_TOKEN=$ten \
		go test -count=1 -run '^TestRemote$' -v "$repo/e2e/"

	step "the audit records the caller's address, not the ingress's"
	me=$(curl -sf https://checkip.amazonaws.com)
	clients=$(api GET "tenants/acme/events?limit=100" "$srv" -f | jq -r '.. | .client? // empty' | sort -u)
	echo "clients: $(echo "$clients" | tr '\n' ' ') (this machine: $me)"
	echo "$clients" | grep -q "^${me%.*}\." || { echo "no event from this machine's address" >&2; exit 1; }

	step "a new revision, a channel's .well-known through the ingress throughout"
	: >"$work/polls"
	rm -f "$work/stop"
	(
		while [ ! -e "$work/stop" ]; do
			curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 "$url/acme/prod/.well-known/duckdb-extension-repo.json" \
				>>"$work/polls" 2>/dev/null || echo fail >>"$work/polls"
			sleep 0.2
		done
	) &
	poller=$!
	az containerapp update -g "$group" -n kista-live --revision-suffix "r$(date +%H%M%S)" -o none
	for i in $(seq 1 60); do
		[ "$(az containerapp show -g "$group" -n kista-live --query properties.runningStatus -o tsv)" = Running ] &&
			[ "$(az containerapp revision list -g "$group" -n kista-live --query "length([?properties.active])" -o tsv)" = 1 ] && break
		sleep 10
	done
	sleep 60
	touch "$work/stop"
	wait "$poller"
	total=$(wc -l <"$work/polls")
	bad=$(grep -cvx 200 "$work/polls" || true)
	echo "polls: $total, not 200: $bad"
	[ "$total" -gt 20 ] && [ "$bad" = 0 ]
	echo
	echo "check: ok"
}

down() {
	group=$(get group)
	[ -n "$group" ] || { echo "down: no group in $work/state" >&2; exit 1; }
	vault=$(out vault 2>/dev/null || true)
	role=$(az role definition list --custom-role-only true --scope "/subscriptions/$(az account show --query id -o tsv)/resourceGroups/$group" \
		--query "[?starts_with(roleName, 'kista key signer ($group/')].name" -o tsv 2>/dev/null || true)
	step "the resource group $group"
	az group exists -n "$group" | grep -q true && az group delete -n "$group" --yes
	if [ -n "$vault" ]; then
		step "the vault $vault, purged"
		az keyvault purge -n "$vault" -l "$location" 2>/dev/null || true
	fi
	if [ -n "$role" ]; then
		step "the custom role"
		az role definition delete --name "$role" --scope "/subscriptions/$(az account show --query id -o tsv)" 2>/dev/null ||
			az role definition delete --name "$role"
	fi
	step "the app registrations"
	for a in "$(get server_app)" "$(get acme_app)"; do
		id=$(az ad app list --display-name "$a" --query '[0].id' -o tsv)
		[ -n "$id" ] && az ad app delete --id "$id"
	done
	rm -f "$work/secret"
}

leftovers() {
	group=$(get group)
	[ -n "$group" ] || { echo "leftovers: no group in $work/state" >&2; exit 1; }
	az group exists -n "$group" | grep -q false || echo "LEFT group $group"
	az resource list --tag "purpose=$name" --query '[].id' -o tsv | sed 's|^|LEFT |'
	az keyvault list-deleted --query "[?starts_with(name, '$name')].name" -o tsv | sed 's|^|LEFT soft-deleted vault |'
	az role definition list --custom-role-only true --query "[?starts_with(roleName, 'kista key signer ($name')].roleName" -o tsv | sed 's|^|LEFT role |'
	az ad app list --display-name "$name" --query '[].displayName' -o tsv | sed 's|^|LEFT app |'
}

case $cmd in
up) up ;;
check) check ;;
down) down ;;
leftovers) leftovers ;;
*) echo "usage: recipe-live.sh up | check | down | leftovers" >&2; exit 2 ;;
esac
