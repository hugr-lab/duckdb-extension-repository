#!/usr/bin/env bash
# A disposable GCP stack for kista's gcpkms and gcs (specs 0004 phase 4, 0005 phase 3b): a project of
# its own, so that down.sh removes everything by deleting the project. Cloud KMS key rings and keys
# can never be deleted, only their versions destroyed; a dedicated project is the only clean way out.
#
#   KISTA_GCP_BILLING=XXXXXX-XXXXXX-XXXXXX deploy/gcp/up.sh [project-id]
#
# Optional: KISTA_GCP_REGION (default europe-west4), KISTA_GCP_FOLDER or KISTA_GCP_ORG (where the
# project goes), KISTA_GCP_HSM=0 for software keys (development only: kista refuses them outside
# profile dev), KISTA_GCP_GITHUB_REPO=owner/repo to let that repository's GitHub Actions use the
# stack through workload identity federation (no keys).
set -euo pipefail

project=${1:-kista-$(date +%y%m%d)-$RANDOM}
case $project in
kista-*) ;;
*) echo "up.sh: the project id must start with kista- (down.sh deletes only such projects)" >&2; exit 2 ;;
esac
: "${KISTA_GCP_BILLING:?set KISTA_GCP_BILLING to the billing account id}"
region=${KISTA_GCP_REGION:-europe-west4}
protection=hsm
hsm="" profile=""
if [ "${KISTA_GCP_HSM:-1}" = 0 ]; then
	protection=software hsm=", require_hsm: false" profile="profile: dev  # software keys"
fi
here=$(cd "$(dirname "$0")" && pwd)
state=$here/.state-$project

parent=()
[ -n "${KISTA_GCP_FOLDER:-}" ] && parent=(--folder "$KISTA_GCP_FOLDER")
[ -n "${KISTA_GCP_ORG:-}" ] && parent=(--organization "$KISTA_GCP_ORG")

echo "up.sh: project $project in $region (keys: $protection)"
gcloud projects create "$project" --name "kista test stack" --labels purpose=kista-test ${parent[@]+"${parent[@]}"}
echo "project=$project" >"$state"
gcloud billing projects link "$project" --billing-account "$KISTA_GCP_BILLING" >/dev/null
gcloud services enable --project "$project" cloudkms.googleapis.com storage.googleapis.com iam.googleapis.com \
	iamcredentials.googleapis.com sts.googleapis.com

# the signing keys: a channel key and an attestation key, RSA 2048 PKCS#1 v1.5 SHA-256, labelled
gcloud kms keyrings create kista --project "$project" --location "$region"
for k in channel:channel-signing attestation:attestation-signing; do
	gcloud kms keys create "ext-${k%%:*}" --project "$project" --keyring kista --location "$region" \
		--purpose asymmetric-signing --default-algorithm rsa-sign-pkcs1-2048-sha256 \
		--protection-level "$protection" --labels "kista-purpose=${k#*:}"
done

# kista's service account, with a role that signs and reads key metadata and nothing else
sa=kista-signer@$project.iam.gserviceaccount.com
gcloud iam service-accounts create kista-signer --project "$project" --display-name kista
# the operator may act as it (no key): deploy/gcp/live.sh gets the live tests' token so
me=$(gcloud config get-value account 2>/dev/null)
member=user:$me
case $me in *.gserviceaccount.com) member=serviceAccount:$me ;; esac
gcloud iam service-accounts add-iam-policy-binding "$sa" --project "$project" \
	--member "$member" --role roles/iam.serviceAccountTokenCreator >/dev/null
gcloud iam roles create kistaSigner --project "$project" --title "kista signer" --permissions \
	cloudkms.cryptoKeyVersions.useToSign,cloudkms.cryptoKeyVersions.viewPublicKey,cloudkms.cryptoKeyVersions.get,cloudkms.cryptoKeys.get
for k in ext-channel ext-attestation; do
	gcloud kms keys add-iam-policy-binding "$k" --project "$project" --keyring kista --location "$region" \
		--member "serviceAccount:$sa" --role "projects/$project/roles/kistaSigner" >/dev/null
done

# the bodies' bucket: private, uniform access
bucket=$project-bodies
gcloud storage buckets create "gs://$bucket" --project "$project" --location "$region" \
	--uniform-bucket-level-access --public-access-prevention
gcloud storage buckets add-iam-policy-binding "gs://$bucket" --member "serviceAccount:$sa" \
	--role roles/storage.objectUser >/dev/null
printf 'region=%s\nsa=%s\nbucket=%s\nsoftware=%s\n' "$region" "$sa" "$bucket" "$([ "$protection" = software ] && echo 1 || echo 0)" >>"$state"

if [ -n "${KISTA_GCP_GITHUB_REPO:-}" ]; then
	number=$(gcloud projects describe "$project" --format 'value(projectNumber)')
	gcloud iam workload-identity-pools create kista --project "$project" --location global
	gcloud iam workload-identity-pools providers create-oidc github --project "$project" --location global \
		--workload-identity-pool kista --issuer-uri https://token.actions.githubusercontent.com \
		--attribute-mapping "google.subject=assertion.sub,attribute.repository=assertion.repository" \
		--attribute-condition "assertion.repository=='$KISTA_GCP_GITHUB_REPO'"
	gcloud iam service-accounts add-iam-policy-binding "$sa" --project "$project" --role roles/iam.workloadIdentityUser \
		--member "principalSet://iam.googleapis.com/projects/$number/locations/global/workloadIdentityPools/kista/attribute.repository/$KISTA_GCP_GITHUB_REPO" >/dev/null
	echo "github_provider=projects/$number/locations/global/workloadIdentityPools/kista/providers/github" >>"$state"
fi

cat <<CONF

up.sh: done. kista's configuration (credentials: the metadata server on GKE/GCE, or an
external_account file through credentials_file):

$profile
signers:
  sources:
    - name: gcp
      kind: gcpkms
      gcpkms: { key_ring: projects/$project/locations/$region/keyRings/kista, principal: $sa$hsm }
      allow: ["ext-"]
# key references: gcp:ext-channel/1 (a channel key), gcp:ext-attestation/1 (an attestation key)
blob:
  domains:
    - name: default
      kind: gcs
      gcs: { bucket: $bucket, principal: $sa }

service account: $sa
live tests: deploy/gcp/live.sh $project
remove everything: deploy/gcp/down.sh $project
CONF
