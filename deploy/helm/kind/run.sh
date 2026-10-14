#!/usr/bin/env bash
# The chart's kind test (spec 0013): a kind cluster with Traefik, PostgreSQL and SeaweedFS (S3) over
# TLS, OpenBao Transit with Kubernetes auth; kista from this checkout, installed by the chart with two
# replicas behind the ingress and the NetworkPolicy. Then:
#   - the shapes of the mounted files kista reads (stat);
#   - the first administration through kubectl exec (a tenant, a signed channel, an OpenBao key, the
#     cluster's service account issuer, a grant, two releases);
#   - DuckDB at e2e/DUCKDB_PIN through the ingress (TestRemote): httpfs over http, a private
#     extension over https, refused without a token and installed with one;
#   - a rolling restart, with /readyz polled through the ingress throughout.
#
#   deploy/helm/kind/run.sh            needs docker, kind, kubectl, helm, openssl, jq, curl, go,
#                                      and the e2e DuckDB build (make e2e-build); DuckDB reaches
#                                      kista.localhost, which must resolve to 127.0.0.1 (macOS and
#                                      systemd-resolved do; else add it to /etc/hosts)
#   KISTA_KIND_KEEP=1 …                keeps the cluster afterwards
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../.." && pwd)
cluster=${KISTA_KIND_CLUSTER:-kista-e2e}
ns=e2e
work=$(mktemp -d)
export KUBECONFIG="$work/kubeconfig"
export GOWORK=off

TRAEFIK_CHART=oci://ghcr.io/traefik/helm/traefik
TRAEFIK_VERSION=41.7.1
POSTGRES=postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24
OPENBAO=openbao/openbao:2.7.1@sha256:6d2b93856e3fcf7b18ad855a0b51eaba474dc8b79cf554379ea32034797d2acf
SEAWEEDFS=chrislusf/seaweedfs:4.48@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d
BUSYBOX=busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e

step() { printf '\n== %s\n' "$*"; }
cleanup() {
	status=$?
	if [ -n "${poller:-}" ]; then kill "$poller" 2>/dev/null || true; fi
	if [ "$status" != 0 ] && kubectl get ns "$ns" >/dev/null 2>&1; then
		echo "== failed: the state of $ns" >&2
		kubectl -n "$ns" get pods -o wide >&2 || true
		kubectl -n "$ns" describe pods -l app.kubernetes.io/name=kista >&2 || true
		kubectl -n "$ns" logs -l app.kubernetes.io/name=kista --tail 200 --prefix >&2 || true
	fi
	[ -n "${KISTA_KIND_KEEP:-}" ] || kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
ka() { kubectl -n "$ns" exec -i deploy/kista -c kista -- /kista admin -config /etc/kista/kista.yaml "$@"; }
curlk() { curl -sS --resolve kista.localhost:443:127.0.0.1 --resolve kista.localhost:80:127.0.0.1 --cacert "$work/ca.crt" "$@"; }

step "the test CA and a certificate for kista.localhost, postgres, seaweedfs and openbao"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj "/CN=kista e2e CA" \
	-keyout "$work/ca.key" -out "$work/ca.crt" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=kista.localhost" \
	-keyout "$work/tls.key" -out "$work/tls.csr" 2>/dev/null
printf 'subjectAltName=DNS:kista.localhost,DNS:postgres.%s.svc,DNS:seaweedfs.%s.svc,DNS:openbao.%s.svc\nextendedKeyUsage=serverAuth\n' "$ns" "$ns" "$ns" >"$work/ext"
openssl x509 -req -in "$work/tls.csr" -CA "$work/ca.crt" -CAkey "$work/ca.key" -CAcreateserial -days 2 \
	-extfile "$work/ext" -out "$work/tls.crt" 2>/dev/null

step "the cluster"
kind create cluster --name "$cluster" --config "$here/kind.yaml" --kubeconfig "$KUBECONFIG" --wait 120s
docker build -q -t kista:e2e "$repo" >/dev/null
kind load docker-image kista:e2e --name "$cluster"
kubectl create ns "$ns"
# the token issuer's JWKS without credentials, as kista fetches it (test only)
kubectl create clusterrolebinding kista-e2e-issuer-discovery --clusterrole system:service-account-issuer-discovery \
	--group system:unauthenticated

step "Traefik"
helm install traefik "$TRAEFIK_CHART" --version "$TRAEFIK_VERSION" -n traefik --create-namespace \
	-f "$here/traefik.yaml" --wait --timeout 5m

step "PostgreSQL, SeaweedFS and OpenBao"
dbpass=$(openssl rand -hex 16)
s3key=kista-e2e
s3secret=$(openssl rand -hex 20)
kubectl -n "$ns" create secret tls kista-ingress-tls --cert "$work/tls.crt" --key "$work/tls.key"
kubectl -n "$ns" create secret tls backend-tls --cert "$work/tls.crt" --key "$work/tls.key"
kubectl -n "$ns" create secret generic kista-db --from-literal password="$dbpass"
kubectl -n "$ns" create secret generic kista-s3 --from-literal access-key="$s3key" --from-literal secret-key="$s3secret"
cat >"$work/s3.json" <<EOF
{"identities": [{"name": "kista", "credentials": [{"accessKey": "$s3key", "secretKey": "$s3secret"}],
  "actions": ["Admin", "Read", "Write", "List", "Tagging"]}]}
EOF
kubectl -n "$ns" create secret generic seaweedfs-s3 --from-file s3.json="$work/s3.json"
# OpenBao in dev mode, plus a TLS listener with the test certificate for kista
cat >"$work/bao.hcl" <<'EOF'
listener "tcp" {
  address       = "0.0.0.0:8200"
  tls_cert_file = "/tls/tls.crt"
  tls_key_file  = "/tls/tls.key"
}
EOF
kubectl -n "$ns" create configmap openbao-config --from-file bao.hcl="$work/bao.hcl"
kubectl -n "$ns" apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: postgres}
spec:
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      securityContext: {fsGroup: 70}
      containers:
        - name: postgres
          image: $POSTGRES
          args: [-c, ssl=on, -c, ssl_cert_file=/tls/tls.crt, -c, ssl_key_file=/tls/tls.key]
          env:
            - {name: POSTGRES_USER, value: kista}
            - {name: POSTGRES_DB, value: kista}
            - {name: POSTGRES_PASSWORD, valueFrom: {secretKeyRef: {name: kista-db, key: password}}}
          ports: [{containerPort: 5432}]
          readinessProbe: {exec: {command: [pg_isready, -U, kista]}, periodSeconds: 2}
          volumeMounts: [{name: tls, mountPath: /tls}]
      volumes: [{name: tls, secret: {secretName: backend-tls, defaultMode: 0440}}]
---
apiVersion: v1
kind: Service
metadata: {name: postgres}
spec: {selector: {app: postgres}, ports: [{port: 5432}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: seaweedfs}
spec:
  selector: {matchLabels: {app: seaweedfs}}
  template:
    metadata: {labels: {app: seaweedfs}}
    spec:
      containers:
        - name: seaweedfs
          image: $SEAWEEDFS
          args: [server, -dir=/data, -ip.bind=0.0.0.0, -master.telemetry=false, -s3, -s3.port=8333, -s3.config=/s3/s3.json,
                 -s3.cert.file=/tls/tls.crt, -s3.key.file=/tls/tls.key]
          ports: [{containerPort: 8333}]
          volumeMounts: [{name: tls, mountPath: /tls}, {name: s3, mountPath: /s3}, {name: data, mountPath: /data}]
      volumes:
        - {name: tls, secret: {secretName: backend-tls}}
        - {name: s3, secret: {secretName: seaweedfs-s3}}
        - {name: data, emptyDir: {}}
---
apiVersion: v1
kind: Service
metadata: {name: seaweedfs}
spec: {selector: {app: seaweedfs}, ports: [{port: 8333}]}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: openbao}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: kista-e2e-openbao-tokenreview}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: system:auth-delegator}
subjects: [{kind: ServiceAccount, name: openbao, namespace: $ns}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: openbao}
spec:
  selector: {matchLabels: {app: openbao}}
  template:
    metadata: {labels: {app: openbao}}
    spec:
      serviceAccountName: openbao
      containers:
        - name: openbao
          image: $OPENBAO
          args: [server, -dev, -dev-root-token-id=root, -dev-listen-address=127.0.0.1:8201,
                 -config=/config/bao.hcl]
          env: [{name: SKIP_SETCAP, value: "true"}]
          ports: [{containerPort: 8200}]
          volumeMounts: [{name: tls, mountPath: /tls}, {name: config, mountPath: /config}]
      volumes:
        - {name: tls, secret: {secretName: backend-tls}}
        - {name: config, configMap: {name: openbao-config}}
---
apiVersion: v1
kind: Service
metadata: {name: openbao}
spec: {selector: {app: openbao}, ports: [{port: 8200}]}
EOF
kubectl -n "$ns" rollout status deploy/postgres deploy/seaweedfs deploy/openbao --timeout 5m

step "the S3 bucket"
weed() { kubectl -n "$ns" exec deploy/seaweedfs -- sh -c "echo '$1' | weed shell" 2>&1; }
for i in $(seq 1 30); do
	weed "s3.bucket.create -name kista" >/dev/null
	weed s3.bucket.list | grep -qw kista && break
	[ "$i" = 30 ] && { weed s3.bucket.list; exit 1; }
	sleep 2
done

step "OpenBao: Transit, a sign-only policy, Kubernetes auth for kista's service account"
bao() { kubectl -n "$ns" exec -i deploy/openbao -- env BAO_ADDR=http://127.0.0.1:8201 BAO_TOKEN=root bao "$@"; }
until bao status >/dev/null 2>&1; do sleep 1; done
bao secrets enable transit >/dev/null
bao write -f transit/keys/ext-prod type=rsa-2048 >/dev/null
bao policy write kista-ext-prod - >/dev/null <<'EOF'
path "transit/keys/ext-prod" {
  capabilities = ["read"]
}
path "transit/sign/ext-prod/sha2-256" {
  capabilities = ["update"]
}
EOF
bao auth enable kubernetes >/dev/null
bao write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc >/dev/null
bao write auth/kubernetes/role/kista bound_service_account_names=kista bound_service_account_namespaces="$ns" \
	audience=openbao token_policies=kista-ext-prod token_ttl=1h >/dev/null

step "the CAs kista trusts: the test CA and the API server's"
kubectl -n "$ns" get cm kube-root-ca.crt -o jsonpath='{.data.ca\.crt}' >"$work/kube-ca.crt"
kubectl -n "$ns" create secret generic kista-ca --from-file ca.crt="$work/ca.crt" --from-file kube-ca.crt="$work/kube-ca.crt"

step "kista"
helm install kista "$repo/deploy/helm/kista" -n "$ns" -f "$here/values.yaml" --wait --timeout 5m
kubectl -n "$ns" get pods -l app.kubernetes.io/name=kista -o wide
ready=$(kubectl -n "$ns" get deploy kista -o jsonpath='{.status.readyReplicas}')
[ "$ready" = 2 ] || { echo "want 2 ready replicas, have $ready" >&2; exit 1; }

step "the shapes of the mounted files (a pod with kista's security context and volumes)"
kubectl -n "$ns" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata: {name: shapes}
spec:
  restartPolicy: Never
  serviceAccountName: kista
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532}
  containers:
    - name: stat
      image: $BUSYBOX
      command: [sh, -c, 'stat -L -c "%n %u %g %a %h" /s/db/password /s/ca/ca.crt /t/token']
      volumeMounts: [{name: db, mountPath: /s/db}, {name: ca, mountPath: /s/ca}, {name: t, mountPath: /t}]
  volumes:
    - {name: db, secret: {secretName: kista-db, defaultMode: 0440}}
    - {name: ca, secret: {secretName: kista-ca, defaultMode: 0440}}
    - name: t
      projected:
        sources: [{serviceAccountToken: {audience: openbao, expirationSeconds: 3600, path: token}}]
EOF
kubectl -n "$ns" wait --for=jsonpath='{.status.phase}'=Succeeded pod/shapes --timeout 2m
kubectl -n "$ns" logs shapes | tee "$work/shapes"
# a Secret: root, group 65532 (fsGroup), 0440, one link; a token: root or 65532, nobody else writes it
grep -qx '/s/db/password 0 65532 440 1' "$work/shapes"
grep -qx '/s/ca/ca.crt 0 65532 440 1' "$work/shapes"
read -r _ uid _ mode _ < <(grep '^/t/token ' "$work/shapes")
case "$uid" in 0 | 65532) ;; *) echo "token owner $uid" >&2; exit 1 ;; esac
case "${mode: -2}" in *[2367]*) echo "token mode $mode: writable by others" >&2; exit 1 ;; esac

step "the first administration, through kubectl exec"
ka tenant create acme
ka channel create acme/prod -kind signed
ka key check -signer bao:ext-prod:v1
ka key add acme/prod -signer bao:ext-prod:v1 -active
duck=$repo/e2e/.build
go build -o "$work/kista" "$repo/cmd/kista"
version=$("$work/kista" ext inspect "$duck/extensions/httpfs.duckdb_extension" | awk -F': *' '/^duckdb version/ {print $2}')
case "$version" in v*) kind_=release ;; *) kind_=dev ;; esac
ka version add "$version" -kind "$kind_"
ka channel versions acme/prod -add "$version"
ka release add acme/prod - -name httpfs <"$duck/extensions/httpfs.duckdb_extension"
ka release add acme/prod - -name loadable_extension_demo -private <"$duck/extensions/loadable_extension_demo.duckdb_extension"
ka issuer add acme -name k8s -url https://kubernetes.default.svc.cluster.local -jwks-uri https://kubernetes.default.svc/openid/v1/jwks
kubectl -n "$ns" create serviceaccount alice
ka grant add acme -principal "subject:k8s|system:serviceaccount:$ns:alice" -verb install -channel prod

step "DuckDB through the ingress"
# both of Traefik's routes (http for DuckDB's bootstrap, https) answer before DuckDB starts
for scheme in http https; do
	for i in $(seq 1 30); do
		[ "$(curlk -o /dev/null -w '%{http_code}' "$scheme://kista.localhost/acme/prod/.well-known/duckdb-extension-repo.json" 2>/dev/null)" = 200 ] && break
		[ "$i" = 30 ] && { echo "the $scheme route never answered" >&2; exit 1; }
		sleep 2
	done
done
curlk -f https://kista.localhost/acme/prod/.well-known/duckdb-extension-repo.json >"$work/wellknown.json"
jq -r '.signature_keys[0]' "$work/wellknown.json" >"$work/channel.pem"
grep -q "BEGIN PUBLIC KEY" "$work/channel.pem" || { cat "$work/wellknown.json"; exit 1; }
code=$(curlk -o /dev/null -w '%{http_code}' https://kista.localhost/acme/prod/.well-known/duckdb-extension-repo.json -H 'X-Forwarded-For: 203.0.113.9')
[ "$code" = 200 ]
KISTA_E2E_DUCKDB=$duck KISTA_E2E_URL=https://kista.localhost KISTA_E2E_CHANNEL=acme/prod KISTA_E2E_CA="$work/ca.crt" \
	KISTA_E2E_KEY="$work/channel.pem" \
	KISTA_E2E_TOKEN=$(kubectl -n "$ns" create token alice --audience https://kista.localhost/acme --duration 1h) \
	go test -count=1 -run '^TestRemote$' -v "$repo/e2e/"

step "the NetworkPolicy: 8443 only from the ingress controller's pods"
# both probes are in kista's pod CIDR, which trusted_proxies trusts: only the policy tells them apart
pod=$(kubectl -n "$ns" get pod -l app.kubernetes.io/component=server -o jsonpath='{.items[0].status.podIP}')
probe() { # probe <namespace> <name> [labels]: whether a pod there reaches 8443
	kubectl -n "$1" run "$2" --restart Never --image "$BUSYBOX" ${3:+--labels "$3"} -- \
		wget -q -T 5 -O /dev/null "http://$pod:8443/readyz" >/dev/null
	kubectl -n "$1" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$2" --timeout 30s >/dev/null 2>&1
}
probe traefik np-allowed app.kubernetes.io/name=traefik || { echo "the ingress controller's pods cannot reach 8443" >&2; exit 1; }
if probe "$ns" np-refused; then
	echo "a pod outside the ingress controller reached 8443" >&2
	exit 1
fi

step "a rolling restart, /readyz through the ingress throughout"
: >"$work/polls"
(
	while [ ! -e "$work/stop" ]; do
		curlk -o /dev/null -w '%{http_code}\n' --max-time 5 https://kista.localhost/readyz >>"$work/polls" 2>/dev/null ||
			echo fail >>"$work/polls"
		sleep 0.2
	done
) &
poller=$!
kubectl -n "$ns" rollout restart deploy/kista
kubectl -n "$ns" rollout status deploy/kista --timeout 5m
touch "$work/stop"
wait "$poller"
total=$(wc -l <"$work/polls")
bad=$(grep -cvx 200 "$work/polls" || true)
echo "polls: $total, not 200: $bad"
[ "$total" -gt 20 ] && [ "$bad" = 0 ]

echo
echo "kind test: ok"
