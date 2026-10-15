#!/usr/bin/env bash
# GCP, live (spec 0020 phase 2): the GKE recipe (deploy/gcp-gke) applied whole in a project of its
# own: kista through the chart on GKE Autopilot, its identity by Workload Identity (no key), signing
# with a Cloud KMS HSM key, bodies in GCS, the store on Cloud SQL by IAM authentication over
# verify-full, behind a GKE Ingress with a Google-managed certificate. `check` runs the kind test's
# checks through the load balancer and the management API: the server administrator and DuckDB's
# tokens come from the cluster's own OIDC issuer.
#
#   deploy/gcp/recipe-live.sh up | check | down | leftovers
#
# `down` deletes the project (the only clean way out for Cloud KMS key rings) and the DNS record;
# `leftovers` confirms the project is DELETE_REQUESTED and the record gone. The run's state is in
# deploy/gcp/.state-live.
#
# Required: KISTA_LIVE_HOST, the name kista is served at (kista.example.com); KISTA_GCP_BILLING (up),
# the billing account for the project. Optional: KISTA_LIVE_ROUTE53_ZONE, a Route 53 hosted zone the
# host is in (example.com): the script writes and removes its A record with the AWS CLI; without it,
# point the host at the address up prints, and up waits until it resolves. KISTA_GCP_REGION
# (europe-west1), KISTA_GCP_FOLDER or KISTA_GCP_ORG (where the project goes), KISTA_LIVE_IMAGE
# (ghcr.io/hugr-lab/kista:edge, pinned by its digest at up). Needs gcloud with
# gke-gcloud-auth-plugin, terraform, kubectl, jq, curl, dig, go; the DuckDB e2e build
# (make e2e-build) for check.
set -euo pipefail

cmd=${1:?usage: recipe-live.sh up | check | down | leftovers}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
export GOWORK=off AWS_PAGER=""
host=${KISTA_LIVE_HOST:?set KISTA_LIVE_HOST to the name kista is served at}
r53=${KISTA_LIVE_ROUTE53_ZONE:-}
region=${KISTA_GCP_REGION:-europe-west1}
name=kista-live
ns=kista
work=$here/.state-live
mkdir -p "$work" && chmod 700 "$work"
export KUBECONFIG=$work/kubeconfig TF_DATA_DIR=$work/terraform

step() { printf '\n== %s\n' "$*"; }
save() { echo "$1=$2" >>"$work/state"; }
get() { sed -n "s/^$1=//p" "$work/state" 2>/dev/null | tail -1; }
tf() { terraform -chdir="$repo/deploy/gcp-gke" "$@"; }
out() { tf output -state="$work/terraform.tfstate" -raw "$1"; }
ka() { kubectl -n "$ns" exec -i deploy/kista -- /kista admin -config /etc/kista/kista.yaml "$@"; }

# the host's A record in the Route 53 zone: UPSERT or DELETE
record() {
	local zoneid ip=$2
	zoneid=$(aws route53 list-hosted-zones-by-name --dns-name "$r53." --query 'HostedZones[0].Id' --output text | sed 's|/hostedzone/||')
	aws route53 change-resource-record-sets --hosted-zone-id "$zoneid" --change-batch "$(jq -cn --arg a "$1" --arg n "$host." --arg ip "$ip" \
		'{Changes: [{Action: $a, ResourceRecordSet: {Name: $n, Type: "A", TTL: 60, ResourceRecords: [{Value: $ip}]}}]}')" \
		--query ChangeInfo.Id --output text
}

up() {
	: "${KISTA_GCP_BILLING:?set KISTA_GCP_BILLING to the billing account id}"
	project=$(get project)
	if [ -z "$project" ]; then
		project=$name-$(date +%m%d)-$RANDOM
		save project "$project"
	fi
	step "the project $project ($region)"
	if ! gcloud projects describe "$project" >/dev/null 2>&1; then
		parent=()
		[ -n "${KISTA_GCP_FOLDER:-}" ] && parent=(--folder "$KISTA_GCP_FOLDER")
		[ -n "${KISTA_GCP_ORG:-}" ] && parent=(--organization "$KISTA_GCP_ORG")
		gcloud projects create "$project" --labels "purpose=$name" "${parent[@]}"
	fi
	gcloud billing projects link "$project" --billing-account "$KISTA_GCP_BILLING" >/dev/null
	gcloud services enable --project "$project" compute.googleapis.com container.googleapis.com sqladmin.googleapis.com \
		servicenetworking.googleapis.com cloudkms.googleapis.com dns.googleapis.com iam.googleapis.com \
		iamcredentials.googleapis.com storage.googleapis.com

	image=$(get image)
	if [ -z "$image" ]; then
		ref=${KISTA_LIVE_IMAGE:-ghcr.io/hugr-lab/kista:edge}
		if [[ $ref != *@sha256:* ]]; then
			repo_path=${ref#ghcr.io/} tagname=${ref##*:}
			repo_path=${repo_path%:*}
			token=$(curl -sf "https://ghcr.io/token?scope=repository:$repo_path:pull" | jq -r .token)
			digest=$(curl -sfI -H "Authorization: Bearer $token" -H 'Accept: application/vnd.oci.image.index.v1+json' \
				"https://ghcr.io/v2/$repo_path/manifests/$tagname" | tr -d '\r' | sed -n 's/^docker-content-digest: //Ip')
			ref=${ref%:*}@$digest
		fi
		image=$ref
		save image "$image"
	fi
	echo "image: $image"

	# the cluster's issuer is known before the cluster: its server administrator is the cluster's ops
	issuer=https://container.googleapis.com/v1/projects/$project/locations/$region/clusters/$name
	save issuer "$issuer"
	cat >"$work/live.tfvars" <<EOF
project             = "$project"
region              = "$region"
prefix              = "$name"
labels              = { purpose = "$name" }
domain              = "$host"
image               = "$image"
chart               = "$repo/deploy/helm/kista"
server_issuers      = "[{name: gke, url: '$issuer', required_claims: {sub: 'system:serviceaccount:$ns:ops'}}]"
server_admins       = "['subject:gke|system:serviceaccount:$ns:ops']"
deletion_protection = false
EOF
	step "the GKE recipe (deploy/gcp-gke): the cluster, Cloud SQL, KMS, GCS, the chart; about 30 minutes"
	GOOGLE_OAUTH_ACCESS_TOKEN=$(gcloud auth print-access-token)
	export GOOGLE_OAUTH_ACCESS_TOKEN
	tf init -input=false -lockfile=readonly >/dev/null
	tf apply -input=false -auto-approve -state="$work/terraform.tfstate" -var-file="$work/live.tfvars"
	gcloud container clusters get-credentials "$name" --project "$project" --region "$region"

	ip=$(out ip)
	step "$host -> $ip"
	if [ -n "$r53" ]; then
		change=$(record UPSERT "$ip")
		aws route53 wait resource-record-sets-changed --id "$change"
	else
		echo "point $host at $ip (an A record); waiting until it resolves"
	fi
	for i in $(seq 1 180); do
		[ "$(dig +short "$host" @8.8.8.8 | tail -1)" = "$ip" ] && break
		[ "$i" = 180 ] && { echo "$host does not resolve to $ip" >&2; exit 1; }
		sleep 10
	done

	step "the Google-managed certificate (tens of minutes after the name resolves)"
	for i in $(seq 1 120); do
		status=$(kubectl -n "$ns" get managedcertificate kista -o jsonpath='{.status.certificateStatus}')
		[ "$status" = Active ] && break
		[ "$i" = 120 ] && { kubectl -n "$ns" describe managedcertificate kista; exit 1; }
		sleep 30
	done
	echo "up: kista at https://$host"
}

api() {
	local method=$1 path=$2 token=$3
	shift 3
	curl -sS -X "$method" -H "Authorization: Bearer $token" "$@" "https://$host/api/v1/$path"
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
	step "$host resolves here, and answers over http and https"
	for i in $(seq 1 120); do
		[ -n "$(dig +short "$host" | head -1)" ] && break
		[ "$i" = 120 ] && { echo "$host does not resolve here" >&2; exit 1; }
		sleep 10
	done
	for scheme in http https; do
		for i in $(seq 1 60); do
			[ "$(curl -s -o /dev/null -w '%{http_code}' "$scheme://$host/readyz")" = 200 ] && break
			[ "$i" = 60 ] && { echo "$scheme://$host/readyz never answered 200" >&2; exit 1; }
			sleep 5
		done
	done

	step "the first administration through the API: the server administrator is GKE's $ns/ops"
	for sa in ops alice; do
		kubectl -n "$ns" get serviceaccount "$sa" >/dev/null 2>&1 || kubectl -n "$ns" create serviceaccount "$sa" >/dev/null
	done
	srv=$(kubectl -n "$ns" create token ops --audience "https://$host" --duration 1h)
	ten=$(kubectl -n "$ns" create token ops --audience "https://$host/acme" --duration 1h)
	api GET whoami "$srv" -f | jq -e '.administrator == true' >/dev/null
	key=$(out channel_signer)
	duck=$repo/e2e/.build
	go build -o "$work/kista" "$repo/cmd/kista"
	field() { "$work/kista" ext inspect "$1" | awk -F': *' -v k="$2" '$1 == k {print $2}'; }
	version=$(field "$duck/extensions/httpfs.duckdb_extension" "duckdb version")
	case "$version" in v*) vkind=release ;; *) vkind=dev ;; esac
	json='Content-Type: application/json'
	ok POST tenants "$srv" -H "$json" -d '{"name":"acme"}'
	ok POST tenants/acme/channels "$srv" -H "$json" -d '{"name":"prod","kind":"signed"}'
	ok POST duckdb-versions "$srv" -H "$json" -d "$(jq -cn --arg n "$version" --arg k "$vkind" '{name: $n, kind: $k}')"
	ok POST tenants/acme/channels/prod/duckdb-versions "$srv" -H "$json" -d "$(jq -cn --arg v "$version" '{duckdb_version: $v}')"
	ok POST tenants/acme/channels/prod/keys "$srv" -H "$json" -d "$(jq -cn --arg s "$key" '{signer_ref: $s, active: true}')"
	ok POST tenants/acme/issuers "$srv" -H "$json" -d "$(jq -cn --arg u "$(get issuer)" '{name: "gke", url: $u}')"
	for g in ops:publish alice:install; do
		ok POST tenants/acme/grants "$srv" -H "$json" -d "$(jq -cn --arg p "subject:gke|system:serviceaccount:$ns:${g%%:*}" \
			--arg v "${g##*:}" '{principal: $p, verbs: [$v], channel: "prod"}')"
	done
	# httpfs is DuckDB's (a reserved name): publishing it needs a grant that names it
	ok POST tenants/acme/grants "$srv" -H "$json" -d "$(jq -cn --arg p "subject:gke|system:serviceaccount:$ns:ops" \
		'{principal: $p, verbs: ["publish"], channel: "prod", extension: "httpfs"}')"

	step "publication through the load balancer: signed by Cloud KMS, stored in GCS"
	for ext in httpfs:public loadable_extension_demo:private; do
		f=$duck/extensions/${ext%%:*}.duckdb_extension
		ok POST "tenants/acme/channels/prod/extensions/${ext%%:*}/releases?version=$(field "$f" "extension version")&platform=$(field "$f" platform)&visibility=${ext##*:}" \
			"$ten" -H 'Content-Type: application/octet-stream' --data-binary "@$f"
	done
	curl -sf "https://$host/acme/prod/.well-known/duckdb-extension-repo.json" >"$work/wellknown.json"
	jq -r '.signature_keys[0]' "$work/wellknown.json" >"$work/channel.pem"
	grep -q "BEGIN PUBLIC KEY" "$work/channel.pem"

	step "an admin command: blob check"
	ka blob check

	step "DuckDB through the load balancer"
	KISTA_E2E_DUCKDB=$duck KISTA_E2E_URL=https://$host KISTA_E2E_CHANNEL=acme/prod KISTA_E2E_CA=/etc/ssl/cert.pem \
		KISTA_E2E_KEY="$work/channel.pem" \
		KISTA_E2E_TOKEN=$(kubectl -n "$ns" create token alice --audience "https://$host/acme" --duration 1h) \
		go test -count=1 -run '^TestRemote$' -v "$repo/e2e/"

	step "the audit records the caller's address, not the load balancer's"
	me=$(curl -sf https://checkip.amazonaws.com)
	clients=$(api GET "tenants/acme/events?limit=100" "$srv" -f | jq -r '.. | .client? // empty' | sort -u)
	echo "clients: $(echo "$clients" | tr '\n' ' ') (this machine: $me)"
	echo "$clients" | grep -q "^${me%.*}\." || { echo "no event from this machine's address" >&2; exit 1; }

	step "a rolling restart, a channel's .well-known through the load balancer throughout"
	: >"$work/polls"
	rm -f "$work/stop"
	(
		while [ ! -e "$work/stop" ]; do
			curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 "https://$host/acme/prod/.well-known/duckdb-extension-repo.json" \
				>>"$work/polls" 2>/dev/null || echo fail >>"$work/polls"
			sleep 0.2
		done
	) &
	poller=$!
	kubectl -n "$ns" rollout restart deploy/kista
	kubectl -n "$ns" rollout status deploy/kista --timeout 15m
	sleep 60 # the old pods leave the NEG and drain
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
	project=$(get project)
	[ -n "$project" ] || { echo "down: no project in $work/state" >&2; exit 1; }
	if [ -n "$r53" ]; then
		step "the DNS record $host"
		ip=$(dig +short "$host" @8.8.8.8 | tail -1)
		[ -n "$ip" ] && { record DELETE "$ip" >/dev/null || true; }
	fi
	step "the project $project, with everything in it (30 days pending deletion, not billed)"
	state=$(gcloud projects describe "$project" --format='value(lifecycleState)' 2>/dev/null || true)
	[ "$state" = ACTIVE ] && gcloud projects delete "$project" --quiet
	rm -rf "$work/terraform" "$work/kubeconfig"
}

leftovers() {
	project=$(get project)
	state=$(gcloud projects describe "$project" --format='value(lifecycleState)' 2>/dev/null || echo gone)
	echo "project $project: $state"
	[ "$state" = DELETE_REQUESTED ] || [ "$state" = gone ]
	gcloud projects list --filter="labels.purpose=$name AND lifecycleState=ACTIVE" --format='value(projectId)'
	if [ -n "$r53" ]; then
		zoneid=$(aws route53 list-hosted-zones-by-name --dns-name "$r53." --query 'HostedZones[0].Id' --output text | sed 's|/hostedzone/||')
		aws route53 list-resource-record-sets --hosted-zone-id "$zoneid" --query "ResourceRecordSets[?Name=='$host.'].[Name,Type]" --output text
	fi
}

case $cmd in
up) up ;;
check) check ;;
down) down ;;
leftovers) leftovers ;;
*) echo "usage: recipe-live.sh up | check | down | leftovers" >&2; exit 2 ;;
esac
