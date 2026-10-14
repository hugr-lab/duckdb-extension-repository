#!/usr/bin/env bash
# Lets one GitHub repository's Actions act as a stack's service account through workload identity
# federation (no key): a pool kista, a provider github that admits only that repository, and
# roles/iam.workloadIdentityUser for it on the service account. up.sh calls it with
# KISTA_GCP_GITHUB_REPO; it can also be run on a stack made without.
#
#   deploy/gcp/github.sh <project-id> <owner/repo>
set -euo pipefail

project=${1:?usage: github.sh <project-id> <owner/repo>}
repo=${2:?usage: github.sh <project-id> <owner/repo>}
case $repo in
*/*) ;;
*) echo "github.sh: the repository is owner/repo, not $repo" >&2; exit 2 ;;
esac
here=$(cd "$(dirname "$0")" && pwd)
state=$here/.state-$project
[ -f "$state" ] || { echo "github.sh: no $state (made by up.sh)" >&2; exit 2; }
sa=$(sed -n 's/^sa=//p' "$state")
number=$(gcloud projects describe "$project" --format 'value(projectNumber)')
pool=projects/$number/locations/global/workloadIdentityPools/kista

gcloud iam workload-identity-pools create kista --project "$project" --location global
gcloud iam workload-identity-pools providers create-oidc github --project "$project" --location global \
	--workload-identity-pool kista --issuer-uri https://token.actions.githubusercontent.com \
	--attribute-mapping "google.subject=assertion.sub,attribute.repository=assertion.repository" \
	--attribute-condition "assertion.repository=='$repo'"
gcloud iam service-accounts add-iam-policy-binding "$sa" --project "$project" --role roles/iam.workloadIdentityUser \
	--member "principalSet://iam.googleapis.com/$pool/attribute.repository/$repo" >/dev/null
echo "github_provider=$pool/providers/github" >>"$state"

cat <<VARS

github.sh: done. The repository variables .github/workflows/gcp-live.yml reads:
  gh variable set KISTA_GCP_PROVIDER --body $pool/providers/github
  gh variable set KISTA_GCP_PRINCIPAL --body $sa
  gh variable set KISTA_GCP_KEYRING --body projects/$project/locations/$(sed -n 's/^region=//p' "$state")/keyRings/kista
  gh variable set KISTA_GCP_BUCKET --body $(sed -n 's/^bucket=//p' "$state")
VARS
