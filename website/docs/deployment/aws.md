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
| **The task role** `<prefix>-task` | no key: `kms:DescribeKey`, `ListResourceTags`, `GetPublicKey` and `Sign` on keys whose tag `kista-purpose` is `channel-signing` or `attestation-signing`; the bucket's objects and multipart uploads; `rds-db:connect` as the database user `kista` |
| **KMS** | a channel key `<prefix>-channel`: `RSA_2048`, `SIGN_VERIFY`, tagged `kista-purpose=channel-signing` |
| **S3** | a private bucket: public access blocked, `BucketOwnerEnforced`, SSE-S3, incomplete uploads aborted after 7 days |
| **RDS** | PostgreSQL 17 with IAM authentication, in private subnets, not publicly accessible, encrypted, with backups; the master password kept by RDS in Secrets Manager |
| **db-init** | a one-time task (`postgres:17-alpine`, its own execution role, the only one that reads the master password) that makes the IAM user `kista` the owner of the database and of schema `public` |
| **ECS** | the service on Fargate in private subnets, the spool on the task's disk, rolled back on a failed deployment |
| **The load balancer** | an ALB with TLS 1.2/1.3 and an ACM certificate, port 80 forwarded without a redirect (DuckDB's bootstrap needs plain http), health checks on `:8081/readyz` |
| **Commands** | a task definition `<prefix>-command` for `kista admin` |
| **The network** | a VPC with two AZs and one NAT gateway, unless `vpc_id` names yours; VPC endpoints with `vpc_endpoints` |

kista's configuration is one document the recipe writes, given to the tasks in the variable
`KISTA_CONFIG` and read with `-config env:KISTA_CONFIG` (Fargate mounts no file). The database's CA
is in it as `store.ca_pem`.

## Deploy

Terraform 1.5 or later (or OpenTofu), the AWS CLI where it runs (`apply` runs `db-init` with it),
and credentials for the account: an IAM user or role of your own, not the account's root.

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
- **`image`** is a version or a digest: `ghcr.io/hugr-lab/kista:vX.Y.Z` or `…@sha256:…`. Fargate
  pulls it from ghcr.io through the NAT gateway.
- **`server_issuers`** and **`server_admins`** are `auth.server_issuers` and `auth.server_admins`
  ([Configuration](../configuration.md#auth)), as YAML in a string: the identity provider whose
  people administer kista.

## The first administration

With a server administrator configured, everything is done through the
[API](../api.md) and the [console](../console.md): tenants, channels, the key, issuers, grants.
The channel's key is the recipe's (`terraform output channel_signer`, `aws:<key id>`):

```sh
curl -X POST -H "Authorization: Bearer $SERVER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"signer_ref": "aws:<key id>", "active": true}' https://kista.example.com/api/v1/tenants/acme/channels/prod/keys
```

## Run a command

```sh
aws ecs run-task --cluster <prefix> --launch-type FARGATE --task-definition <prefix>-command \
  --network-configuration "awsvpcConfiguration={subnets=[<private subnets>],securityGroups=[<task_security_group>],assignPublicIp=DISABLED}" \
  --overrides '{"containerOverrides":[{"name":"kista","command":["admin","-config","env:KISTA_CONFIG","blob","check"]}]}'
```

The command is the binary's arguments after `/kista`: `admin -config env:KISTA_CONFIG` and a
command such as `migrate`, `blob check`, `blob verify`, `blob gc -apply`. Its log is in CloudWatch
Logs (`terraform output log_group`). Who may run tasks with this definition, pass the task role or
change the service acts as kista: keep that to the platform's operators.

## The network

- The tasks and the database are in private subnets; the load balancer is the only public
  endpoint, and the tasks' security group admits only it, on 8443 and 8081.
- `serve.trusted_proxies` is the CIDRs of the load balancer's subnets: ALB nodes change addresses
  within them. The audit records the caller's address, not the load balancer's.
- The load balancer's idle timeout is 60 seconds, below kista's 120-second keep-alive, so it never
  reuses a connection kista closed.
- A NAT gateway is needed in any case: kista pulls its image, and reaches its issuers and
  upstreams. With `vpc_endpoints`, KMS, S3, Secrets Manager and Logs stay inside the VPC.

## Limits

- **The spool.** It needs (2 × `max_ingests` + 1) × `max_body` of the task's disk;
  `ephemeral_storage_gib` (21 to 200) must hold it with 4 GiB to spare, or the plan fails.
- **RDS IAM tokens** are signed for the database's region (`aws.region`); a database in another
  region is refused by the configuration check.

## For production

- `desired_count` 2 or more, a larger `db_instance_class`, `multi_az` on the database (in
  `main.tf`), one NAT gateway per AZ.
- `deletion_protection` stays on (the database, the load balancer, the bucket's contents).
- **Tagging is trust.** Any principal allowed `kms:TagResource` in the account can tag a key
  `kista-purpose` and make it usable by kista: deny tagging `kista-purpose` with an SCP or a
  permissions boundary.
- **The master user** stays an operator's break-glass login, its password RDS's own in Secrets
  Manager. Do not grant it the `kista` role: a role that holds `kista` inherits `rds_iam`, and RDS
  then refuses its password.
- Pin the image by digest.

## On EKS with the chart

The same configuration in the chart's values (`config`): `aws: {region, identity: {kind: irsa,
role_arn: …}}`, `store.login: {kind: aws}`, `store.ca_pem`, an `awskms` source and an `s3` domain
with the same identity, and `aws.irsa.roleArn` in the values. Behind the AWS Load Balancer
Controller, set `preStopSeconds: 20` and label the namespace for the controller's readiness gate
([Kubernetes](kubernetes.md)). The database needs the same one-time grants as `db-init` above.

## Destroy

`deletion_protection = false`, `apply`, then `terraform destroy`. The KMS key is scheduled for
deletion (`key_deletion_days`, 7 to 30): cancel it with `aws kms cancel-key-deletion` if that was a
mistake, since every signature the channel served depends on it.

## Account set-up

Observed on new AWS accounts, before anything of kista's runs:

- An organisation's service control policy may allow only some services in member accounts;
  check that ECS, EC2, RDS, KMS, S3 and Route 53 are allowed.
- IAM Identity Center sourced from AWS Builder ID cannot assign accounts; an IAM user with a
  short-lived key, deleted afterwards, works.
