#!/usr/bin/env bash
# Azure, live (spec 0020 phase 4b): the Marketplace package (deploy/azure-marketplace) as a
# service-catalog managed application, the Marketplace's path without Partner Center. A definition
# with lock None and a Reader authorization for a principal other than the runner; an application
# from it with the answers the UI would give (server administrators and the first tenant's by an
# Entra app role); `check` proves the bootstrap made the tenant, the attestation key opened for its
# purpose, no deny assignment, and the customer (the runner) starting a job.
#
#   deploy/azure/managed-app-live.sh up | check | down | leftovers
#
# `down` deletes the application (its managed resource group with it), the group, purges the vault
# and deletes the app registrations; `leftovers` lists whatever of the run's remains. The run's
# state is in deploy/azure/.state-managed (the client secret among it, mode 0600; it expires in two
# days and goes with its app).
#
# Optional: KISTA_AZURE_LOCATION (northeurope), KISTA_AZURE_DATABASE (postgres or sqlserver),
# KISTA_LIVE_IMAGE (ghcr.io/hugr-lab/kista:edge, pinned by its digest at up). The subscription is
# az's current one; the signed-in account needs Owner of the subscription's groups and may create
# app registrations. Needs az with bicep, jq, curl, zip.
set -euo pipefail

cmd=${1:?usage: managed-app-live.sh up | check | down | leftovers}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
location=${KISTA_AZURE_LOCATION:-northeurope}
database=${KISTA_AZURE_DATABASE:-postgres}
name=kista-ma
work=$here/.state-managed
mkdir -p "$work" && chmod 700 "$work"
reader=acdd72a7-3385-48ef-bd42-f606fba81ae7 # Reader

step() { printf '\n== %s\n' "$*"; }
save() { echo "$1=$2" >>"$work/state"; }
get() { [ -f "$work/state" ] && sed -n "s/^$1=//p" "$work/state" | tail -1 || true; }
claims() { local p=${1#*.}; p=${p%%.*}; p=$(printf '%s' "$p" | tr '_-' '/+'); while [ $((${#p} % 4)) != 0 ]; do p="$p="; done; printf '%s' "$p" | base64 -d; }
graph() { az rest --method "$1" --uri "https://graph.microsoft.com/v1.0/$2" --headers Content-Type=application/json ${3:+--body "$3"}; }

# an app registration with v2.0 tokens, an api://<app id> identifier and one application role
# (value $2), assigned to the client app's service principal: the client's tokens for it carry the
# role in roles, as the Marketplace UI expects
app() {
	local display=$1 role=$2 id oid
	id=$(az ad app list --display-name "$display" --query '[0].appId' -o tsv)
	[ -n "$id" ] || id=$(az ad app create --display-name "$display" --query appId -o tsv)
	oid=$(az ad app show --id "$id" --query id -o tsv)
	# a new app is not always visible to Graph at once; its role is checked, not assumed
	for i in $(seq 1 12); do
		[ "$(az ad app show --id "$id" --query "length(appRoles[?value=='$role'])" -o tsv)" = 1 ] && break
		graph PATCH "applications/$oid" "$(jq -cn --arg u "api://$id" --arg r "$role" --arg rid "$(uuidgen | tr A-Z a-z)" \
			'{api: {requestedAccessTokenVersion: 2}, identifierUris: [$u],
			  appRoles: [{id: $rid, value: $r, displayName: $r, description: $r, allowedMemberTypes: ["Application"], isEnabled: true}]}')" >/dev/null 2>&1 || true
		[ "$i" = 12 ] && { echo "up: the app $display has no role $role" >&2; return 1; }
		sleep 10
	done
	az ad sp show --id "$id" >/dev/null 2>&1 || az ad sp create --id "$id" >/dev/null
	echo "$id"
}

# assigns the app role of resource app $1 to the client's service principal
assign() {
	local res=$1 rsp csp rid
	rsp=$(az ad sp show --id "$res" --query id -o tsv)
	csp=$(az ad sp show --id "$(get client)" --query id -o tsv)
	rid=$(az ad app show --id "$res" --query 'appRoles[0].id' -o tsv)
	graph GET "servicePrincipals/$rsp/appRoleAssignedTo" | jq -e --arg c "$csp" '.value[] | select(.principalId == $c)' >/dev/null ||
		graph POST "servicePrincipals/$rsp/appRoleAssignedTo" "$(jq -cn --arg p "$csp" --arg r "$rsp" --arg a "$rid" \
			'{principalId: $p, resourceId: $r, appRoleId: $a}')" >/dev/null
}

token() {
	curl -sf "https://login.microsoftonline.com/$(get tenant)/oauth2/v2.0/token" \
		--data-urlencode grant_type=client_credentials --data-urlencode "client_id=$(get client)" \
		--data-urlencode "client_secret@$work/secret" --data-urlencode "scope=api://$1/.default" | jq -r .access_token
}

managed_group() { echo "$(get group)-managed"; }
output() { az managedapp show -g "$(get group)" -n kista --query "outputs.$1.value" -o tsv; }

up() {
	group=$(get group)
	if [ -z "$group" ]; then
		group=$name-$(date +%m%d)-$RANDOM
		save group "$group"
	fi
	tenant=$(az account show --query tenantId -o tsv)
	sub=$(az account show --query id -o tsv)
	save tenant "$tenant"

	step "Entra: the server's and tenant main's API apps, each with a role the client holds"
	server=$(app "$group-server" kista-admin)
	main=$(app "$group-main" kista-tenant-admin)
	save client "$server"
	save server_app "$server"
	save main_app "$main"
	assign "$server"
	assign "$main"
	if [ ! -s "$work/secret" ]; then
		(umask 077; az ad app credential reset --id "$server" --display-name live --end-date "$(date -u -v+2d +%Y-%m-%d 2>/dev/null || date -u -d +2days +%Y-%m-%d)" \
			--query password -o tsv | tr -d '\n' >"$work/secret")
	fi
	for i in $(seq 1 30); do
		t=$(token "$server" 2>/dev/null || true)
		[ -n "$t" ] && [ "$t" != null ] && claims "$t" | jq -e '.roles | index("kista-admin")' >/dev/null && break
		[ "$i" = 30 ] && { echo "up: no token with the role kista-admin" >&2; exit 1; }
		sleep 10
	done
	iss=$(claims "$t" | jq -r .iss)
	save iss "$iss"

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

	step "the package"
	"$repo/deploy/azure-marketplace/build.sh" "$image" "$work/app.zip"
	rm -rf "$work/app" && mkdir "$work/app" && (cd "$work/app" && unzip -q ../app.zip)

	step "the group $group ($location) and the service-catalog definition"
	az group create -n "$group" -l "$location" --tags purpose=kista-live run=kista-ma -o none
	readerOid=$(az ad sp show --id "$main" --query id -o tsv) # a principal other than the runner
	az managedapp definition create -g "$group" -n kista-def -l "$location" --lock-level None \
		--display-name "kista (live)" --description "kista, the Marketplace package, live" \
		--authorizations "$readerOid:$reader" \
		--main-template @"$work/app/mainTemplate.json" --create-ui-definition @"$work/app/createUiDefinition.json" -o none
	def=$(az managedapp definition show -g "$group" -n kista-def --query id -o tsv)

	step "the application, with the UI's answers; about 20 minutes"
	jq -n --arg loc "$location" --arg db "$database" --arg iss "$iss" --arg tid "$tenant" --arg server "$server" --arg main "$main" '{
		location: {value: $loc}, prefix: {value: "kistama"}, database: {value: $db},
		cpu: {value: "2.0"}, replicas: {value: 1}, maxBodyMib: {value: 512}, maxIngests: {value: 2}, attestationKey: {value: true},
		serverIssuers: {value: [{name: "entra", url: $iss, required_claims: {tid: $tid}, roles_claim: "roles"}]},
		serverAudiences: {value: [$server]}, serverAdmins: {value: ["role:entra|kista-admin"]},
		serverConsoleClients: {value: [{issuer: "entra", client_id: $server, scopes: ["openid", "profile", "offline_access", ($server + "/.default")]}]},
		bootstrap: {value: {
			versions: [{version: "v2.0.0", kind: "release", c_apis: ["v1.5.6"]}],
			tenant: {name: "main", audiences: [$main]},
			channel: {name: "prod", versions: ["v2.0.0"]},
			issuer: {name: "entra", url: $iss, required_claims: {tid: $tid}, roles_claim: "roles"},
			grants: [{principal: "role:entra|kista-tenant-admin", verbs: ["admin"]}]}},
		purgeProtection: {value: false}, softDeleteDays: {value: 7}, tags: {value: {purpose: "kista-live", run: "kista-ma"}}
	}' >"$work/parameters.json"
	az managedapp create -g "$group" -n kista -l "$location" --kind ServiceCatalog \
		--managed-rg-id "/subscriptions/$sub/resourceGroups/$(managed_group)" --managedapp-definition-id "$def" \
		--parameters @"$work/parameters.json" -o none
	echo "up: kista at $(output address)"
}

check() {
	trap 'rm -f "$work/auth"' EXIT
	url=$(output address)
	mrg=$(managed_group)
	step "$url answers"
	for i in $(seq 1 60); do
		[ "$(curl -s -o /dev/null -w '%{http_code}' "$url/readyz")" = 200 ] && break
		[ "$i" = 60 ] && { echo "$url/readyz never answered 200" >&2; exit 1; }
		sleep 5
	done

	step "the bootstrap made tenant main: its channel serves the channel key"
	curl -sf "$url/main/prod/.well-known/duckdb-extension-repo.json" | jq -e '.signature_keys | length == 1' >/dev/null

	step "a server administrator by app role; a tenant administrator by the tenant's"
	srv=$(token "$(get server_app)")
	ten=$(token "$(get main_app)")
	(umask 077; printf 'Authorization: Bearer %s\n' "$srv" >"$work/auth")
	curl -sf -H @"$work/auth" "$url/api/v1/whoami" | jq -e '.administrator == true' >/dev/null
	curl -sf -H @"$work/auth" "$url/api/v1/tenants/main/events?kind=bootstrap.apply" |
		jq -e '[.events[] | select(.actor == "system:bootstrap")] | length == 1' >/dev/null
	curl -sf -H @"$work/auth" "$url/api/v1/tenants/main/events?kind=attestation_key.add" | jq -e '.events | length == 1' >/dev/null
	(umask 077; printf 'Authorization: Bearer %s\n' "$ten" >"$work/auth")
	curl -sf -H @"$work/auth" "$url/api/v1/tenants/main/channels" | jq -e '.channels[0].name == "prod"' >/dev/null

	step "no deny assignment on the managed group; the customer starts a job"
	sub=$(az account show --query id -o tsv)
	n=$(az rest --method GET --uri "https://management.azure.com/subscriptions/$sub/resourceGroups/$mrg/providers/Microsoft.Authorization/denyAssignments?api-version=2022-04-01" --query 'length(value)' -o tsv)
	[ "$n" = 0 ] || { echo "deny assignments on $mrg: $n" >&2; exit 1; }
	execution=$(az containerapp job start -g "$mrg" -n kistama-bootstrap --query name -o tsv)
	for i in $(seq 1 60); do
		status=$(az containerapp job execution show -g "$mrg" -n kistama-bootstrap --job-execution-name "$execution" --query properties.status -o tsv)
		case "$status" in Succeeded) break ;; Failed | Stopped | Degraded) echo "bootstrap job: $status" >&2; exit 1 ;; esac
		[ "$i" = 60 ] && { echo "bootstrap job: still $status" >&2; exit 1; }
		sleep 10
	done
	echo
	echo "check: ok"
}

down() {
	group=$(get group)
	[ -n "$group" ] || { echo "down: no group in $work/state" >&2; exit 1; }
	mrg=$(managed_group)
	vault=$(az keyvault list -g "$mrg" --query '[0].name' -o tsv 2>/dev/null || true)
	step "the application and its managed group"
	az managedapp show -g "$group" -n kista >/dev/null 2>&1 && az managedapp delete -g "$group" -n kista
	step "the group $group"
	if az group exists -n "$group" | grep -q true; then az group delete -n "$group" --yes; fi
	if [ -n "$vault" ]; then
		step "the vault $vault, purged"
		az keyvault purge -n "$vault"
	fi
	step "the app registrations"
	for a in "$group-server" "$group-main"; do
		id=$(az ad app list --display-name "$a" --query '[0].id' -o tsv)
		[ -z "$id" ] || az ad app delete --id "$id"
	done
	rm -f "$work/secret" "$work/auth" "$work/parameters.json"
}

leftovers() {
	group=$(get group)
	[ -n "$group" ] || { echo "leftovers: no group in $work/state" >&2; exit 1; }
	for g in "$group" "$(managed_group)"; do az group exists -n "$g" | grep -q false || echo "LEFT group $g"; done
	az resource list --tag run=kista-ma --query '[].id' -o tsv | sed 's|^|LEFT |'
	az keyvault list-deleted --query "[?starts_with(name, 'kistama')].name" -o tsv | sed 's|^|LEFT soft-deleted vault |'
	az ad app list --display-name "$name" --query '[].displayName' -o tsv | sed 's|^|LEFT app |'
}

case $cmd in
up) up ;;
check) check ;;
down) down ;;
leftovers) leftovers ;;
*) echo "usage: managed-app-live.sh up | check | down | leftovers" >&2; exit 2 ;;
esac
