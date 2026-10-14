#!/usr/bin/env bash
# kista on GKE, by hand (spec 0013): adds a GKE Autopilot cluster to a stack up.sh made and installs
# the chart with GKE Workload Identity (the metadata server, spec 0004 phase 4), a GKE Ingress with
# NEG and the chart's BackendConfig (the load balancer checks /readyz on the health port), the
# NetworkPolicy for Google's front ends, and SQLite on a persistent disk. Then, through kubectl exec:
# blob check (GCS), a tenant, a channel and its Cloud KMS key; and through the load balancer:
# /readyz and the channel's .well-known. down.sh removes the cluster with the project.
#
#   deploy/gcp/gke.sh <project-id>
#
# Optional: KISTA_IMAGE (default ghcr.io/hugr-lab/kista:edge, or a digest reference; the package must
# be public, the chart here sets no pull secret).
set -euo pipefail

project=${1:?usage: gke.sh <project-id>}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
state=$here/.state-$project
[ -f "$state" ] || { echo "gke.sh: no $state (made by up.sh)" >&2; exit 2; }
region=$(sed -n 's/^region=//p' "$state")
sa=$(sed -n 's/^sa=//p' "$state")
bucket=$(sed -n 's/^bucket=//p' "$state")
software=$(sed -n 's/^software=//p' "$state")
image=${KISTA_IMAGE:-ghcr.io/hugr-lab/kista:edge}
ns=kista
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export KUBECONFIG=$here/.kubeconfig-$project

echo "gke.sh: an Autopilot cluster in $project ($region)"
gcloud services enable --project "$project" container.googleapis.com compute.googleapis.com
if ! gcloud container clusters describe kista --project "$project" --region "$region" >/dev/null 2>&1; then
	gcloud container clusters create-auto kista --project "$project" --region "$region"
fi
gcloud container clusters get-credentials kista --project "$project" --region "$region"
echo "kubeconfig=$KUBECONFIG" >>"$state"

# the chart's service account acts as kista-signer through Workload Identity
gcloud iam service-accounts add-iam-policy-binding "$sa" --project "$project" \
	--role roles/iam.workloadIdentityUser --member "serviceAccount:$project.svc.id.goog[$ns/kista]" >/dev/null

# ghcr.io/hugr-lab/kista:edge or …@sha256:…; a tag is after the last ':' past the last '/'
repository=$image tag=edge digest=""
if [[ $image == *@* ]]; then
	repository=${image%@*} digest=${image#*@}
elif [[ ${image##*/} == *:* ]]; then
	repository=${image%:*} tag=${image##*:}
fi
profile=""
hsm=""
[ "$software" = 1 ] && profile="profile: dev" hsm="require_hsm: false"
dev=false
[ "$software" = 1 ] && dev=true
cat >"$work/values.yaml" <<EOF
devProfile: $dev
image:
  repository: $repository
  tag: "$tag"
  digest: "$digest"
  pullPolicy: Always
gcp:
  serviceAccount: $sa
config:
  $profile
  store:
    kind: sqlite
  serve:
    public_url: https://kista.example.com
    # Google's front ends: the load balancer's proxies and its health checks
    trusted_proxies: [35.191.0.0/16, 130.211.0.0/22]
  signers:
    sources:
      - name: gcp
        kind: gcpkms
        allow: ["ext-"]
        gcpkms:
          key_ring: projects/$project/locations/$region/keyRings/kista
          principal: $sa
          $hsm
  blob:
    domains:
      - name: default
        kind: gcs
        gcs: {bucket: $bucket, principal: $sa}
gke:
  backendConfig:
    enabled: true
ingress:
  enabled: true
  # GKE's controller takes an Ingress by this annotation (Autopilot has no IngressClass object)
  annotations:
    kubernetes.io/ingress.class: gce
  hosts: [{}]
networkPolicy:
  loadBalancerRanges: [35.191.0.0/16, 130.211.0.0/22]
EOF
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install kista "$repo/deploy/helm/kista" -n "$ns" -f "$work/values.yaml" --wait --timeout 15m

ka() { kubectl -n "$ns" exec -i deploy/kista -- /kista admin -config /etc/kista/kista.yaml "$@"; }
ka blob check
# idempotent, so the script can run again
[[ $(ka tenant list) == *acme* ]] || ka tenant create acme
[[ $(ka channel list acme) == *prod* ]] || ka channel create acme/prod -kind signed
[[ $(ka key list acme/prod) == *gcp:ext-channel/1* ]] || ka key add acme/prod -signer gcp:ext-channel/1 -active

echo "gke.sh: waiting for the load balancer (several minutes)"
for i in $(seq 1 90); do
	ip=$(kubectl -n "$ns" get ingress kista -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
	[ -n "$ip" ] && curl -sf -o /dev/null "http://$ip/readyz" && break
	[ "$i" = 90 ] && { kubectl -n "$ns" describe ingress kista; exit 1; }
	sleep 10
done
curl -sf "http://$ip/readyz"
echo
curl -sf "http://$ip/acme/prod/.well-known/duckdb-extension-repo.json"
echo
cat <<EOF

gke.sh: kista answers through the load balancer at http://$ip, signing with Cloud KMS and storing
in GCS through GKE Workload Identity.
  kubectl: KUBECONFIG=$KUBECONFIG kubectl -n $ns get pods
  remove everything: deploy/gcp/down.sh $project
EOF
