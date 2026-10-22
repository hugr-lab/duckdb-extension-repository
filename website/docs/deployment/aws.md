---
title: AWS
---

# AWS

Two ways to run kista on AWS, both without a static credential of the service's own:

- **ECS on Fargate**, with the Terraform recipe in
  [`deploy/aws-ecs`](https://github.com/hugr-lab/duckdb-extension-repository/tree/main/deploy/aws-ecs):
  the service, its database, its key, its bucket and its load balancer in one `terraform apply`.
- **EKS**, with the [Helm chart](kubernetes.md) and IRSA or EKS Pod Identity. The settings below
  (the database login, the key, the bucket) are the same; the chart's page covers the cluster's
  side.

The design is in
[spec 0020](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0020-cloud-recipes/spec.md).

## What the recipe makes

| | |
| --- | --- |
| **The task role** `<prefix>-task` | no key: `kms:DescribeKey`, `ListResourceTags`, `GetPublicKey` and `Sign` on keys tagged `kista-purpose` (`channel-signing` or `attestation-signing`) and `kista-deployment=<prefix>`; the bucket's objects and multipart uploads; `rds-db:connect` as the database user `kista` |
| **KMS** | a channel key `<prefix>-channel`: `RSA_2048`, `SIGN_VERIFY`, tagged as above |
| **S3** | a private bucket: public access blocked, `BucketOwnerEnforced`, SSE-S3, requests without TLS denied, incomplete uploads aborted after 7 days |
| **RDS** | PostgreSQL 17 with IAM authentication, in private subnets, not publicly accessible, encrypted, with backups; the master password kept by RDS in Secrets Manager |
| **db-init** | a one-time task (`postgres:17-alpine`, its own execution role, the only one that reads the master password, `verify-full` to the database) that makes the IAM user `kista` the owner of the database and of schema `public` |
| **ECS** | the service on Fargate in private subnets, all capabilities dropped, the spool on the task's disk, rolled back on a failed deployment |
| **The load balancer** | an ALB with TLS 1.2/1.3 and an ACM certificate; port 80 forwarded without a redirect (DuckDB's bootstrap needs plain http) except `/api` and `/console`, which go to https; health checks on `:8081/readyz` |
| **Commands** | a task definition `<prefix>-command` for `kista admin` |
| **The network** | a VPC with two AZs and one NAT gateway, unless `vpc_id` names yours; VPC endpoints with `vpc_endpoints` |

kista's configuration is one document the recipe writes, given to the tasks in the variable
`KISTA_CONFIG` and read with `-config env:KISTA_CONFIG` (Fargate mounts no file). Whoever may edit
the task definition decides every setting, as whoever may edit a mounted file would. The
database's CA is in it as `store.ca_pem`: the region's RDS bundle, fetched from AWS's trust store
over https at plan time, or given as `rds_ca_pem` (the regional bundle; the global one would
overflow the task definition).

## Deploy

Terraform 1.5 or later (or OpenTofu), bash and the AWS CLI where it runs (`apply` runs `db-init`
with them), https to `truststore.pki.rds.amazonaws.com` at plan time unless `rds_ca_pem` is given,
and credentials for the account: an IAM user or role of your own, not the account's root. Keep
the state in a remote, encrypted, access-controlled backend: it holds no secret, but it maps the
deployment.

```sh
cd deploy/aws-ecs
cp examples/trial.tfvars my.tfvars   # domain, route53_zone_id, image, server_issuers, server_admins
terraform init
terraform apply -var-file my.tfvars
terraform output url
```

- **`domain`** is kista's public URL. With **`route53_zone_id`** the recipe validates the
  certificate and points the name at the load balancer. A domain elsewhere: give
  `certificate_arn` (an issued ACM certificate) and point the name at `lb_dns_name` yourself, or
  delegate a subdomain to a Route 53 zone once.
- **`image`** is a version or a digest: `ghcr.io/hugr-lab/kista:vX.Y.Z` or `…@sha256:…`. Moving tags
  (`:edge`, `:latest`) are refused unless pinned by digest. Fargate pulls the image from ghcr.io
  through the NAT gateway.
- **`server_issuers`** and **`server_admins`** are `auth.server_issuers` and `auth.server_admins`
  ([Configuration](../configuration.md#auth)), as YAML in a string: the identity provider whose
  people administer kista. A server issuer needs `required_claims`.
- **`server_license_admins`** is `auth.server_license_admins`, as YAML in a string: the
  [licence managers](../licensing.md#licences-in-a-tenant), principals of the server issuers who
  manage every tenant's licences and nothing else. Empty (the default), there are none, and the
  configuration leaves the key out.
- **`server_distribution_admins`** is `auth.server_distribution_admins`, as YAML in a string: the
  [distribution admins](../access.md), principals of the server issuers who reserve names in tenants
  and manage the publishers they mark. Empty (the default), there are none, and the configuration
  leaves the key out.

## The first administration

With a server administrator configured, everything is done through the [API](../api.md) and the
[console](../console.md): tenants, channels, the key, issuers, grants. The channel's key is the
recipe's (`terraform output channel_signer`, `aws:<key id>`):

```sh
curl -X POST -H "Authorization: Bearer $SERVER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"signer_ref": "aws:<key id>", "active": true}' https://kista.example.com/api/v1/tenants/acme/channels/prod/keys
```

Another key for this deployment (another channel, or a rotation) carries both tags:

```sh
aws kms create-key --key-spec RSA_2048 --key-usage SIGN_VERIFY \
  --tags TagKey=kista-purpose,TagValue=channel-signing TagKey=kista-deployment,TagValue=<prefix>
```

## Run a command

```sh
aws ecs run-task --cluster "$(terraform output -raw cluster)" --launch-type FARGATE \
  --task-definition "$(terraform output -raw command_task_definition)" \
  --network-configuration "awsvpcConfiguration={subnets=[$(terraform output -json private_subnet_ids | jq -r 'join(",")')],securityGroups=[$(terraform output -raw task_security_group)],assignPublicIp=DISABLED}" \
  --overrides '{"containerOverrides":[{"name":"kista","command":["admin","-config","env:KISTA_CONFIG","blob","check"]}]}'
```

The command is the binary's arguments after `/kista`: `admin -config env:KISTA_CONFIG` and a
command such as `migrate`, `blob check`, `blob verify`, `blob gc -apply` (without an override, the
definition runs `admin … check`). Its log is in CloudWatch Logs (`terraform output log_group`).

Who may run tasks with these definitions, pass the task role, or change the service acts as kista.
Who may pass the `db-init` execution role to a task of their own can read the database's master
password. Keep both to the platform's operators.

## The network

- The tasks and the database are in private subnets; the load balancer is the only public
  endpoint, and the tasks' security group admits only it, on 8443 and 8081. The tasks reach out on
  443 and to the database only.
- `serve.trusted_proxies` is the CIDRs of the load balancer's subnets: ALB nodes change addresses
  within them. The audit records the caller's address, not the load balancer's. This rests on the
  tasks' security group: widening it lets other hosts in those subnets claim any client address.
- Tokens travel over https only. Port 80 serves DuckDB's bootstrap and public releases; the API and
  the console redirect to https, and kista ignores a token sent over plain http.
- The load balancer's idle timeout is 60 seconds, below kista's 120-second keep-alive, so it never
  reuses a connection kista closed.
- A NAT gateway is needed in any case: kista pulls its image, and reaches its issuers and
  upstreams. With `vpc_endpoints`, KMS, Secrets Manager and Logs stay inside the VPC, and S3 too in
  the recipe's own VPC or, in yours, on `private_route_table_ids`.

## Limits

- **The spool.** It needs (2 × `max_ingests` + 1) × `max_body_mib` of the task's disk;
  `ephemeral_storage_gib` (21 to 200) must hold it with 4 GiB to spare, or the plan fails.
- **RDS IAM tokens** are signed for the database's region (`aws.region`); a database in another
  region is refused by the configuration check.
- **The root filesystem** of the tasks is writable: a Fargate bind mount at the spool's path is
  root-owned, and kista runs as uid 65532. The binary is root-owned and the image has no shell.

## For production

- Keep `desired_count` at 2 or more; a larger `db_instance_class`, `multi_az` on the database (in
  `main.tf`), one NAT gateway per AZ.
- `deletion_protection` stays on: the database and the load balancer refuse a destroy. It does not
  cover the KMS key: a destroy schedules the key's deletion (`key_deletion_days`, 7 to 30), and
  every signature the channel served depends on it. Cancel it with `aws kms cancel-key-deletion` if
  that was a mistake.
- **Tagging is trust.** Any principal allowed `kms:TagResource` in the account can tag a key
  `kista-purpose` and `kista-deployment` and make it usable by this kista: deny tagging those with
  an SCP or a permissions boundary.
- **The master user** stays an operator's break-glass login, its password RDS's own in Secrets
  Manager (account administrators who may read every secret can read it). Do not grant it the
  `kista` role: a role that holds `kista` inherits `rds_iam`, and RDS then refuses its password.
- Bucket versioning with a noncurrent-version expiry guards against deletion by a compromised task
  role (garbage collection deletes bodies, so the role may).
- Pin the image by digest.

## On EKS with the chart

The same configuration in the chart's values (`config`): `aws: {region, identity: {kind: irsa,
role_arn: …}}`, `store.login: {kind: aws}`, `store.ca_pem`, an `awskms` source and an `s3` domain
with the same identity, and `aws.irsa.roleArn` in the values. Behind the AWS Load Balancer
Controller, set `preStopSeconds: 20` and label the namespace for the controller's readiness gate
([Kubernetes](kubernetes.md)). The database needs the same one-time grants as `db-init` above.

## Destroy

`deletion_protection = false`, `apply`, then `terraform destroy`. The KMS key is scheduled for
deletion (see above).

## Account set-up

Observed on new AWS accounts, before anything of kista's runs:

- An organisation's service control policy may allow only some services in member accounts;
  check that ECS, EC2, RDS, KMS, S3 and Route 53 are allowed.
- IAM Identity Center sourced from AWS Builder ID cannot assign accounts; an IAM user with a
  short-lived key, deleted afterwards, works.
