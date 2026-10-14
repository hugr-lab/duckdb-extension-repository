#!/usr/bin/env bash
# Runs the live tests of gcpkms and gcs against a stack up.sh made, as its service account: the
# token comes from impersonation (up.sh lets its operator do that), no key is made.
#
#   deploy/gcp/live.sh <project-id> [go test flags]
set -euo pipefail

project=${1:?usage: live.sh <project-id> [go test flags]}
shift
here=$(cd "$(dirname "$0")" && pwd)
state=$here/.state-$project
[ -f "$state" ] || { echo "live.sh: no $state (made by up.sh)" >&2; exit 2; }
region=$(sed -n 's/^region=//p' "$state")
sa=$(sed -n 's/^sa=//p' "$state")
bucket=$(sed -n 's/^bucket=//p' "$state")
software=$(sed -n 's/^software=//p' "$state")

# a fresh grant takes a minute or so to reach IAM Credentials
for i in $(seq 1 12); do
	token=$(gcloud auth print-access-token --impersonate-service-account "$sa" 2>/dev/null) && break
	[ "$i" = 12 ] && { echo "live.sh: cannot act as $sa" >&2; exit 1; }
	sleep 10
done

cd "$here/../.."
KISTA_LIVE_GCP_TOKEN=$token KISTA_LIVE_GCP_SOFTWARE=$software \
	KISTA_LIVE_GCP_KEYRING=projects/$project/locations/$region/keyRings/kista KISTA_LIVE_GCS_BUCKET=$bucket \
	GOWORK=off go test -tags live -count=1 -run Live "$@" ./internal/keysource/gcpkms/ ./internal/blob/gcs/
