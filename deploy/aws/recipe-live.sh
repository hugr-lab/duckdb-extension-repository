#!/usr/bin/env bash
# AWS, live (spec 0020 phase 1): kista through the chart on EKS, its identity by IRSA (no key of the
# service's own): signing with an AWS KMS key, bodies in S3, the store on RDS PostgreSQL by IAM
# authentication over verify-full, behind an ALB with an ACM certificate. `check` runs the kind
# test's checks through the load balancer: DuckDB installs a private extension with a token from the
# cluster's own OIDC issuer.
#
#   deploy/aws/recipe-live.sh up | check | down | leftovers
#
# Every resource carries the tag purpose=kista-live and names starting kista-live. `down` deletes
# them all (the KMS key is scheduled for deletion, 7 days); `leftovers` lists whatever remains and
# must print nothing but the key pending deletion. The run's state is in deploy/aws/.state-live.
#
# Required: KISTA_LIVE_ZONE, a Route 53 hosted zone of the account (example.com) for the ALB's name
# and its ACM certificate. Optional: AWS_REGION (eu-central-1), KISTA_LIVE_HOST
# (kista.$KISTA_LIVE_ZONE), KISTA_LIVE_NODE (t4g.medium: arm64 nodes, so the image builds natively
# on Apple silicon). Needs aws, eksctl, kubectl, helm, docker, jq, go; the
# DuckDB e2e build (make e2e-build) for check.
set -euo pipefail

cmd=${1:?usage: recipe-live.sh up | check | down | leftovers}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
export AWS_REGION=${AWS_REGION:-eu-central-1} AWS_PAGER=""
zone=${KISTA_LIVE_ZONE:?set KISTA_LIVE_ZONE to a Route 53 hosted zone of the account}
host=${KISTA_LIVE_HOST:-kista.$zone}
node=${KISTA_LIVE_NODE:-t4g.medium}
name=kista-live
ns=kista
tag="Key=purpose,Value=$name"
work=$here/.state-live
mkdir -p "$work" && chmod 700 "$work"
export KUBECONFIG=$work/kubeconfig
account=$(aws sts get-caller-identity --query Account --output text)
# the AWS Load Balancer Controller, pinned; its IAM policy from the same tag
lbc_version=v3.6.0

step() { printf '\n== %s\n' "$*"; }
save() { echo "$1=$2" >>"$work/state"; }
get() { sed -n "s/^$1=//p" "$work/state" 2>/dev/null | tail -1; }
ka() { kubectl -n "$ns" exec -i deploy/kista -- /kista admin -config /etc/kista/kista.yaml "$@"; }

up() {
	step "the image, built from this checkout, in ECR"
	aws ecr describe-repositories --repository-names "$name" >/dev/null 2>&1 ||
		aws ecr create-repository --repository-name "$name" --tags "$tag" --image-scanning-configuration scanOnPush=false >/dev/null
	registry=$account.dkr.ecr.$AWS_REGION.amazonaws.com
	aws ecr get-login-password | docker login --username AWS --password-stdin "$registry" >/dev/null
	docker buildx build --platform linux/arm64 --build-arg VERSION="live-$(git -C "$repo" rev-parse --short HEAD)" \
		-t "$registry/$name:live" --push "$repo"
	digest=$(aws ecr describe-images --repository-name "$name" --image-ids imageTag=live --query 'imageDetails[0].imageDigest' --output text)
	save image "$registry/$name"
	save digest "$digest"

	step "the cluster (eksctl: a VPC, two $node nodes, the OIDC provider for IRSA), about 20 minutes"
	if ! eksctl get cluster --name "$name" >/dev/null 2>&1; then
		eksctl create cluster --name "$name" --nodegroup-name ng --node-type "$node" --nodes 2 --managed --with-oidc \
			--tags "purpose=$name" --kubeconfig "$KUBECONFIG"
	fi
	eksctl utils write-kubeconfig --cluster "$name" --kubeconfig "$KUBECONFIG" >/dev/null
	vpc=$(aws eks describe-cluster --name "$name" --query cluster.resourcesVpcConfig.vpcId --output text)
	cidr=$(aws ec2 describe-vpcs --vpc-ids "$vpc" --query 'Vpcs[0].CidrBlock' --output text)
	clustersg=$(aws eks describe-cluster --name "$name" --query cluster.resourcesVpcConfig.clusterSecurityGroupId --output text)
	issuer=$(aws eks describe-cluster --name "$name" --query cluster.identity.oidc.issuer --output text)
	save vpc "$vpc"
	save cidr "$cidr"
	save issuer "$issuer"

	step "KMS: the channel key (RSA_2048, SIGN_VERIFY, kista-purpose=channel-signing)"
	key=$(aws kms describe-key --key-id "alias/$name-channel" --query KeyMetadata.KeyId --output text 2>/dev/null || true)
	if [ -z "$key" ] || [ "$key" = None ]; then
		key=$(aws kms create-key --description "kista live (recipe-live.sh)" --key-spec RSA_2048 --key-usage SIGN_VERIFY \
			--tags TagKey=kista-purpose,TagValue=channel-signing TagKey=purpose,TagValue=$name --query KeyMetadata.KeyId --output text)
		aws kms create-alias --alias-name "alias/$name-channel" --target-key-id "$key"
	fi
	save key "$key"

	step "S3: a private bucket"
	bucket=$name-$account
	if ! aws s3api head-bucket --bucket "$bucket" 2>/dev/null; then
		aws s3api create-bucket --bucket "$bucket" --create-bucket-configuration LocationConstraint="$AWS_REGION" >/dev/null
		aws s3api put-public-access-block --bucket "$bucket" --public-access-block-configuration \
			BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
		aws s3api put-bucket-ownership-controls --bucket "$bucket" --ownership-controls 'Rules=[{ObjectOwnership=BucketOwnerEnforced}]'
		aws s3api put-bucket-tagging --bucket "$bucket" --tagging "TagSet=[{Key=purpose,Value=$name}]"
		aws s3api put-bucket-lifecycle-configuration --bucket "$bucket" --lifecycle-configuration \
			'{"Rules":[{"ID":"abort-incomplete","Status":"Enabled","Filter":{},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":7}}]}'
	fi
	save bucket "$bucket"

	step "RDS: PostgreSQL 17 with IAM authentication, private, verify-full (about 10 minutes)"
	subnets=$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc" "Name=tag:kubernetes.io/role/internal-elb,Values=1" \
		--query 'Subnets[].SubnetId' --output text)
	aws rds describe-db-subnet-groups --db-subnet-group-name "$name" >/dev/null 2>&1 ||
		aws rds create-db-subnet-group --db-subnet-group-name "$name" --db-subnet-group-description "kista live" \
			--subnet-ids $subnets --tags "$tag" >/dev/null
	dbsg=$(aws ec2 describe-security-groups --filters "Name=vpc-id,Values=$vpc" "Name=group-name,Values=$name-db" \
		--query 'SecurityGroups[0].GroupId' --output text)
	if [ "$dbsg" = None ]; then
		dbsg=$(aws ec2 create-security-group --group-name "$name-db" --description "kista live database" --vpc-id "$vpc" \
			--tag-specifications "ResourceType=security-group,Tags=[{$tag}]" --query GroupId --output text)
		aws ec2 authorize-security-group-ingress --group-id "$dbsg" --protocol tcp --port 5432 --source-group "$clustersg" >/dev/null
	fi
	save dbsg "$dbsg"
	if ! aws rds describe-db-instances --db-instance-identifier "$name" >/dev/null 2>&1; then
		pgv=$(aws rds describe-db-engine-versions --engine postgres --engine-version 17 --query 'DBEngineVersions[-1].EngineVersion' --output text)
		aws rds create-db-instance --db-instance-identifier "$name" --engine postgres --engine-version "$pgv" \
			--db-instance-class db.t4g.micro --allocated-storage 20 --storage-encrypted --db-name kista \
			--master-username kistaadmin --manage-master-user-password --enable-iam-database-authentication \
			--db-subnet-group-name "$name" --vpc-security-group-ids "$dbsg" --no-publicly-accessible \
			--backup-retention-period 0 --no-deletion-protection --ca-certificate-identifier rds-ca-rsa2048-g1 \
			--tags "$tag" >/dev/null
	fi
	aws rds wait db-instance-available --db-instance-identifier "$name"
	endpoint=$(aws rds describe-db-instances --db-instance-identifier "$name" --query 'DBInstances[0].Endpoint.Address' --output text)
	resid=$(aws rds describe-db-instances --db-instance-identifier "$name" --query 'DBInstances[0].DbiResourceId' --output text)
	secret=$(aws rds describe-db-instances --db-instance-identifier "$name" --query 'DBInstances[0].MasterUserSecret.SecretArn' --output text)
	save endpoint "$endpoint"
	curl -sf "https://truststore.pki.rds.amazonaws.com/$AWS_REGION/$AWS_REGION-bundle.pem" >"$work/rds-ca.pem"
	grep -q "BEGIN CERTIFICATE" "$work/rds-ca.pem"

	step "db-init: the IAM user kista, owner of the database and of schema public"
	kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$ns" delete secret db-init pod/db-init --ignore-not-found >/dev/null
	aws secretsmanager get-secret-value --secret-id "$secret" --query SecretString --output text |
		jq -r .password | kubectl -n "$ns" create secret generic db-init --from-file=password=/dev/stdin >/dev/null
	kubectl -n "$ns" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: db-init}
spec:
  restartPolicy: Never
  containers:
    - name: psql
      image: postgres:17-alpine
      env:
        - {name: PGPASSWORD, valueFrom: {secretKeyRef: {name: db-init, key: password}}}
      command: [psql, "host=$endpoint dbname=kista user=kistaadmin sslmode=require", -v, ON_ERROR_STOP=1, -c]
      args:
        - |
          DO \$\$ BEGIN
            IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kista') THEN CREATE ROLE kista LOGIN; END IF;
          END \$\$;
          GRANT rds_iam TO kista;
          GRANT kista TO kistaadmin;
          ALTER DATABASE kista OWNER TO kista;
          ALTER SCHEMA public OWNER TO kista;
EOF
	kubectl -n "$ns" wait --for=jsonpath='{.status.phase}'=Succeeded pod/db-init --timeout 3m ||
		{ kubectl -n "$ns" logs db-init; exit 1; }
	kubectl -n "$ns" delete secret db-init >/dev/null
	kubectl -n "$ns" delete pod db-init >/dev/null

	step "IRSA: kista's role (the key by its purpose tag, the bucket, rds-db:connect as kista)"
	cat >"$work/policy.json" <<EOF
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": ["kms:DescribeKey", "kms:ListResourceTags", "kms:GetPublicKey", "kms:Sign"],
   "Resource": "arn:aws:kms:$AWS_REGION:$account:key/*",
   "Condition": {"StringEquals": {"aws:ResourceTag/kista-purpose": ["channel-signing", "attestation-signing"]}}},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
   "Resource": "arn:aws:s3:::$bucket/*"},
  {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:ListBucketMultipartUploads"], "Resource": "arn:aws:s3:::$bucket"},
  {"Effect": "Allow", "Action": "rds-db:connect", "Resource": "arn:aws:rds-db:$AWS_REGION:$account:dbuser:$resid/kista"}
]}
EOF
	policy=arn:aws:iam::$account:policy/$name-kista
	if aws iam get-policy --policy-arn "$policy" >/dev/null 2>&1; then
		aws iam list-policy-versions --policy-arn "$policy" --query 'Versions[?!IsDefaultVersion].VersionId' --output text |
			xargs -n1 -I{} aws iam delete-policy-version --policy-arn "$policy" --version-id {} 2>/dev/null || true
		aws iam create-policy-version --policy-arn "$policy" --policy-document "file://$work/policy.json" --set-as-default >/dev/null
	else
		aws iam create-policy --policy-name "$name-kista" --policy-document "file://$work/policy.json" --tags "$tag" >/dev/null
	fi
	eksctl create iamserviceaccount --cluster "$name" --namespace "$ns" --name kista --role-name "$name-kista" --role-only \
		--attach-policy-arn "$policy" --approve >/dev/null
	role=arn:aws:iam::$account:role/$name-kista
	save role "$role"

	step "the AWS Load Balancer Controller $lbc_version"
	lbcpolicy=arn:aws:iam::$account:policy/$name-lbc
	if ! aws iam get-policy --policy-arn "$lbcpolicy" >/dev/null 2>&1; then
		curl -sfL "https://raw.githubusercontent.com/kubernetes-sigs/aws-load-balancer-controller/$lbc_version/docs/install/iam_policy.json" >"$work/lbc.json"
		aws iam create-policy --policy-name "$name-lbc" --policy-document "file://$work/lbc.json" --tags "$tag" >/dev/null
	fi
	eksctl create iamserviceaccount --cluster "$name" --namespace kube-system --name aws-load-balancer-controller \
		--role-name "$name-lbc" --attach-policy-arn "$lbcpolicy" --override-existing-serviceaccounts --approve >/dev/null
	helm repo add eks https://aws.github.io/eks-charts >/dev/null 2>&1 || true
	helm repo update eks >/dev/null
	helm upgrade --install aws-load-balancer-controller eks/aws-load-balancer-controller -n kube-system \
		--version "$(helm search repo eks/aws-load-balancer-controller --versions -o json | jq -r --arg v "${lbc_version#v}" '[.[] | select(.app_version == $v)][0].version')" \
		--set clusterName="$name" --set serviceAccount.create=false --set serviceAccount.name=aws-load-balancer-controller \
		--set region="$AWS_REGION" --set vpcId="$vpc" --wait --timeout 5m >/dev/null

	step "ACM: a certificate for $host, validated in $zone"
	zoneid=$(aws route53 list-hosted-zones-by-name --dns-name "$zone." --query 'HostedZones[0].Id' --output text | sed 's|/hostedzone/||')
	save zoneid "$zoneid"
	cert=$(aws acm list-certificates --query "CertificateSummaryList[?DomainName=='$host'].CertificateArn | [0]" --output text)
	if [ "$cert" = None ]; then
		cert=$(aws acm request-certificate --domain-name "$host" --validation-method DNS --tags "$tag" --query CertificateArn --output text)
		sleep 10
	fi
	save cert "$cert"
	read -r rname rvalue < <(aws acm describe-certificate --certificate-arn "$cert" \
		--query 'Certificate.DomainValidationOptions[0].ResourceRecord.[Name,Value]' --output text)
	aws route53 change-resource-record-sets --hosted-zone-id "$zoneid" --change-batch "{\"Changes\":[{\"Action\":\"UPSERT\",
		\"ResourceRecordSet\":{\"Name\":\"$rname\",\"Type\":\"CNAME\",\"TTL\":300,\"ResourceRecords\":[{\"Value\":\"$rvalue\"}]}}]}" >/dev/null
	save validation "$rname $rvalue"
	aws acm wait certificate-validated --certificate-arn "$cert"

	step "kista: the chart, the configuration from the environment-free ConfigMap, IRSA everywhere"
	{
		cat <<EOF
image: {repository: $(get image), digest: "$(get digest)", pullPolicy: Always}
replicaCount: 2
aws: {irsa: {roleArn: $role}}
networkPolicy: {enabled: false}
ingress:
  enabled: true
  className: alb
  annotations:
    alb.ingress.kubernetes.io/scheme: internet-facing
    alb.ingress.kubernetes.io/target-type: ip
    alb.ingress.kubernetes.io/listen-ports: '[{"HTTP": 80}, {"HTTPS": 443}]'
    alb.ingress.kubernetes.io/certificate-arn: $cert
    alb.ingress.kubernetes.io/ssl-policy: ELBSecurityPolicy-TLS13-1-2-2021-06
    alb.ingress.kubernetes.io/healthcheck-port: "8081"
    alb.ingress.kubernetes.io/healthcheck-path: /readyz
    alb.ingress.kubernetes.io/tags: purpose=$name
  hosts: [{host: $host}]
config:
  aws:
    region: $AWS_REGION
    identity: {kind: irsa, role_arn: $role}
  store:
    kind: postgres
    dsn: postgres://kista@$endpoint:5432/kista?sslmode=verify-full
    login: {kind: aws}
    ca_pem: |
EOF
		sed 's/^/      /' "$work/rds-ca.pem"
		cat <<EOF
  serve:
    public_url: https://$host
    trusted_proxies: [$cidr]
  signers:
    sources:
      - name: aws
        kind: awskms
        allow: ["*"]
        awskms: {region: $AWS_REGION, account: "$account"}
        identity: {kind: irsa, role_arn: $role}
  blob:
    domains:
      - name: default
        kind: s3
        s3:
          endpoint: https://s3.$AWS_REGION.amazonaws.com
          bucket: $bucket
          region: $AWS_REGION
          lookup: dns
          sse: s3
          account: "$account"
          identity: {kind: irsa, role_arn: $role}
EOF
	} >"$work/values.yaml"
	helm upgrade --install kista "$repo/deploy/helm/kista" -n "$ns" -f "$work/values.yaml" --wait --timeout 10m

	step "DNS: $host to the load balancer"
	for i in $(seq 1 60); do
		lb=$(kubectl -n "$ns" get ingress kista -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
		[ -n "$lb" ] && break
		[ "$i" = 60 ] && { kubectl -n "$ns" describe ingress kista; exit 1; }
		sleep 5
	done
	lbzone=$(aws elbv2 describe-load-balancers --query "LoadBalancers[?DNSName=='$lb'].CanonicalHostedZoneId | [0]" --output text)
	aws route53 change-resource-record-sets --hosted-zone-id "$zoneid" --change-batch "{\"Changes\":[{\"Action\":\"UPSERT\",
		\"ResourceRecordSet\":{\"Name\":\"$host\",\"Type\":\"A\",\"AliasTarget\":{\"HostedZoneId\":\"$lbzone\",
		\"DNSName\":\"dualstack.$lb\",\"EvaluateTargetHealth\":false}}}]}" >/dev/null
	save lb "$lb"
	echo "up: done; next: recipe-live.sh check"
}

check() {
	step "the load balancer answers (http and https, no redirect)"
	for scheme in http https; do
		for i in $(seq 1 60); do
			[ "$(curl -s -o /dev/null -w '%{http_code}' "$scheme://$host/readyz" 2>/dev/null)" = 200 ] && break
			[ "$i" = 60 ] && { echo "$scheme://$host/readyz never answered 200" >&2; exit 1; }
			sleep 5
		done
	done

	step "the first administration: a tenant, a channel, the KMS key, releases, the cluster's issuer, a grant"
	key=$(get key)
	duck=$repo/e2e/.build
	go build -o "$work/kista" "$repo/cmd/kista"
	version=$("$work/kista" ext inspect "$duck/extensions/httpfs.duckdb_extension" | awk -F': *' '/^duckdb version/ {print $2}')
	case "$version" in v*) vkind=release ;; *) vkind=dev ;; esac
	[[ $(ka tenant list) == *acme* ]] || ka tenant create acme
	[[ $(ka channel list acme) == *prod* ]] || ka channel create acme/prod -kind signed
	ka key check -signer "aws:$key"
	[[ $(ka key list acme/prod) == *"aws:$key"* ]] || ka key add acme/prod -signer "aws:$key" -active
	[[ $(ka version list) == *"$version"* ]] || ka version add "$version" -kind "$vkind"
	ka channel versions acme/prod -add "$version" >/dev/null
	ka release add acme/prod - -name httpfs <"$duck/extensions/httpfs.duckdb_extension" || true
	ka release add acme/prod - -name loadable_extension_demo -private <"$duck/extensions/loadable_extension_demo.duckdb_extension" || true
	[[ $(ka issuer list acme) == *eks* ]] || ka issuer add acme -name eks -url "$(get issuer)"
	kubectl -n "$ns" get serviceaccount alice >/dev/null 2>&1 || kubectl -n "$ns" create serviceaccount alice
	[[ $(ka grant list acme) == *alice* ]] || ka grant add acme -principal "subject:eks|system:serviceaccount:$ns:alice" -verb install -channel prod

	step "the bodies in S3, signed by KMS"
	ka blob check
	ka blob verify
	curl -sf "https://$host/acme/prod/.well-known/duckdb-extension-repo.json" >"$work/wellknown.json"
	jq -r '.signature_keys[0]' "$work/wellknown.json" >"$work/channel.pem"
	grep -q "BEGIN PUBLIC KEY" "$work/channel.pem"

	step "DuckDB through the load balancer"
	KISTA_E2E_DUCKDB=$duck KISTA_E2E_URL=https://$host KISTA_E2E_CHANNEL=acme/prod KISTA_E2E_CA=/etc/ssl/cert.pem \
		KISTA_E2E_KEY="$work/channel.pem" \
		KISTA_E2E_TOKEN=$(kubectl -n "$ns" create token alice --audience "https://$host/acme" --duration 1h) \
		go test -count=1 -run '^TestRemote$' -v "$repo/e2e/"

	step "the audit records the caller's address, not the load balancer's"
	me=$(curl -sf https://checkip.amazonaws.com)
	ka events list acme -format jsonl -limit 50 >"$work/events.jsonl"
	clients=$(jq -r 'select(.client != null and .client != "") | .client' "$work/events.jsonl" | sort -u)
	echo "clients: $(echo "$clients" | tr '\n' ' ') (this machine: $me)"
	echo "$clients" | grep -q "^${me%.*}\." || { echo "no event from this machine's address" >&2; exit 1; }

	step "a rolling restart, /readyz through the load balancer throughout"
	: >"$work/polls"
	rm -f "$work/stop"
	(
		while [ ! -e "$work/stop" ]; do
			curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 "https://$host/readyz" >>"$work/polls" 2>/dev/null || echo fail >>"$work/polls"
			sleep 0.2
		done
	) &
	poller=$!
	kubectl -n "$ns" rollout restart deploy/kista
	kubectl -n "$ns" rollout status deploy/kista --timeout 10m
	sleep 20 # the load balancer drains the old targets
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
	step "kista and its load balancer"
	if [ -f "$KUBECONFIG" ] && kubectl get ns "$ns" >/dev/null 2>&1; then
		helm uninstall kista -n "$ns" --wait 2>/dev/null || true
		for i in $(seq 1 60); do
			n=$(aws elbv2 describe-load-balancers --query "length(LoadBalancers[?VpcId=='$(get vpc)'])" --output text 2>/dev/null || echo 0)
			[ "$n" = 0 ] && break
			sleep 5
		done
	fi
	zoneid=$(get zoneid)
	if [ -n "$zoneid" ]; then
		step "DNS records"
		aws route53 list-resource-record-sets --hosted-zone-id "$zoneid" \
			--query "ResourceRecordSets[?Name=='$host.' || starts_with(Name, '_') && Type=='CNAME']" --output json >"$work/records.json"
		jq -c '.[]' "$work/records.json" | while read -r rr; do
			aws route53 change-resource-record-sets --hosted-zone-id "$zoneid" \
				--change-batch "{\"Changes\":[{\"Action\":\"DELETE\",\"ResourceRecordSet\":$rr}]}" >/dev/null || true
		done
	fi

	step "RDS"
	if aws rds describe-db-instances --db-instance-identifier "$name" >/dev/null 2>&1; then
		aws rds delete-db-instance --db-instance-identifier "$name" --skip-final-snapshot --delete-automated-backups >/dev/null || true
		aws rds wait db-instance-deleted --db-instance-identifier "$name"
	fi
	aws rds delete-db-subnet-group --db-subnet-group-name "$name" 2>/dev/null || true
	dbsg=$(get dbsg)
	[ -n "$dbsg" ] && aws ec2 delete-security-group --group-id "$dbsg" 2>/dev/null || true

	step "the cluster (its VPC, node group, IRSA roles), about 15 minutes"
	if eksctl get cluster --name "$name" >/dev/null 2>&1; then
		helm uninstall aws-load-balancer-controller -n kube-system 2>/dev/null || true
		eksctl delete cluster --name "$name" --wait
	fi

	step "IAM policies, the key, the bucket, the image, the certificate"
	for p in kista lbc; do
		arn=arn:aws:iam::$account:policy/$name-$p
		for r in $(aws iam list-entities-for-policy --policy-arn "$arn" --query 'PolicyRoles[].RoleName' --output text 2>/dev/null); do
			aws iam detach-role-policy --role-name "$r" --policy-arn "$arn" || true
		done
		aws iam list-policy-versions --policy-arn "$arn" --query 'Versions[?!IsDefaultVersion].VersionId' --output text 2>/dev/null |
			xargs -n1 -I{} aws iam delete-policy-version --policy-arn "$arn" --version-id {} 2>/dev/null || true
		aws iam delete-policy --policy-arn "$arn" 2>/dev/null || true
	done
	for r in "$name-kista" "$name-lbc"; do
		aws iam get-role --role-name "$r" >/dev/null 2>&1 || continue
		for a in $(aws iam list-attached-role-policies --role-name "$r" --query 'AttachedPolicies[].PolicyArn' --output text); do
			aws iam detach-role-policy --role-name "$r" --policy-arn "$a"
		done
		aws iam delete-role --role-name "$r"
	done
	key=$(aws kms describe-key --key-id "alias/$name-channel" --query KeyMetadata.KeyId --output text 2>/dev/null || true)
	if [ -n "$key" ] && [ "$key" != None ]; then
		aws kms delete-alias --alias-name "alias/$name-channel"
		aws kms schedule-key-deletion --key-id "$key" --pending-window-in-days 7 >/dev/null
	fi
	bucket=$name-$account
	if aws s3api head-bucket --bucket "$bucket" 2>/dev/null; then
		aws s3 rm "s3://$bucket" --recursive >/dev/null
		aws s3api delete-bucket --bucket "$bucket"
	fi
	aws ecr delete-repository --repository-name "$name" --force >/dev/null 2>&1 || true
	cert=$(get cert)
	[ -n "$cert" ] && aws acm delete-certificate --certificate-arn "$cert" 2>/dev/null || true
	echo "down: done; next: recipe-live.sh leftovers"
}

leftovers() {
	step "tagged purpose=$name (the KMS key pending deletion is expected)"
	aws resourcegroupstaggingapi get-resources --tag-filters "Key=purpose,Values=$name" \
		--query 'ResourceTagMappingList[].ResourceARN' --output text | tr '\t' '\n' | while read -r arn; do
		[ -z "$arn" ] && continue
		case "$arn" in
		arn:aws:kms:*)
			state=$(aws kms describe-key --key-id "$arn" --query KeyMetadata.[KeyState,DeletionDate] --output text 2>/dev/null)
			echo "expected: $arn ($state)"
			;;
		*) echo "LEFT: $arn" ;;
		esac
	done
	step "by name"
	aws iam list-roles --query "Roles[?starts_with(RoleName, '$name')].RoleName" --output text | tr '\t' '\n' | sed '/^$/d; s/^/LEFT iam role: /'
	aws iam list-policies --scope Local --query "Policies[?starts_with(PolicyName, '$name')].PolicyName" --output text | tr '\t' '\n' | sed '/^$/d; s/^/LEFT iam policy: /'
	aws iam list-open-id-connect-providers --query 'OpenIDConnectProviderList[].Arn' --output text | tr '\t' '\n' | while read -r p; do
		[ -z "$p" ] && continue
		[ "$(aws iam list-open-id-connect-provider-tags --open-id-connect-provider-arn "$p" --query "Tags[?Key=='alpha.eksctl.io/cluster-name'].Value | [0]" --output text)" = "$name" ] &&
			echo "LEFT oidc provider: $p"
	done
	aws eks list-clusters --query "clusters[?starts_with(@, '$name')]" --output text | sed '/^$/d; s/^/LEFT eks: /'
	aws cloudformation list-stacks --stack-status-filter CREATE_COMPLETE UPDATE_COMPLETE DELETE_FAILED ROLLBACK_COMPLETE \
		--query "StackSummaries[?contains(StackName, '$name')].StackName" --output text | tr '\t' '\n' | sed '/^$/d; s/^/LEFT stack: /'
	aws rds describe-db-instances --query "DBInstances[?starts_with(DBInstanceIdentifier, '$name')].DBInstanceIdentifier" --output text | sed '/^$/d; s/^/LEFT rds: /'
	aws rds describe-db-snapshots --query "DBSnapshots[?starts_with(DBInstanceIdentifier, '$name')].DBSnapshotIdentifier" --output text | sed '/^$/d; s/^/LEFT snapshot: /'
	aws elbv2 describe-load-balancers --query "LoadBalancers[?contains(LoadBalancerName, 'kista')].LoadBalancerName" --output text | sed '/^$/d; s/^/LEFT lb: /'
	aws ec2 describe-vpcs --filters "Name=tag:alpha.eksctl.io/cluster-name,Values=$name" --query 'Vpcs[].VpcId' --output text | sed '/^$/d; s/^/LEFT vpc: /'
	aws ec2 describe-addresses --filters "Name=tag:alpha.eksctl.io/cluster-name,Values=$name" --query 'Addresses[].AllocationId' --output text | sed '/^$/d; s/^/LEFT eip: /'
	aws ecr describe-repositories --query "repositories[?starts_with(repositoryName, '$name')].repositoryName" --output text 2>/dev/null | sed '/^$/d; s/^/LEFT ecr: /'
	aws acm list-certificates --query "CertificateSummaryList[?DomainName=='$host'].CertificateArn" --output text | sed '/^$/d; s/^/LEFT cert: /'
	aws logs describe-log-groups --query "logGroups[?contains(logGroupName, '$name')].logGroupName" --output text | sed '/^$/d; s/^/LEFT log group: /'
	zoneid=$(aws route53 list-hosted-zones-by-name --dns-name "$zone." --query 'HostedZones[0].Id' --output text | sed 's|/hostedzone/||')
	aws route53 list-resource-record-sets --hosted-zone-id "$zoneid" --query "ResourceRecordSets[?Name=='$host.'].Name" --output text | sed '/^$/d; s/^/LEFT dns: /'
	echo "leftovers: done (anything marked LEFT is still there)"
}

case $cmd in
up | check | down | leftovers) "$cmd" ;;
*) echo "usage: recipe-live.sh up | check | down | leftovers" >&2; exit 2 ;;
esac
