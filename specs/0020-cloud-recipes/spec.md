# Spec 0020: Cloud recipes: AWS and GCP in Terraform, Azure in Bicep

- **Status**: accepted; phase 0 implemented; phases 1-4 to come
- **Date**: 2026-10-14
- **Author**: vgsml, Claude

## Summary

kista runs on Kubernetes through its chart (spec 0013). This spec adds one **recipe** per cloud. A
recipe deploys kista on that cloud's managed services, using the cloud's own identity and no live
static secret. Each recipe is run live once before its phase is marked done.

| Cloud | Recipe | Where it runs | Store | Keys | Bodies |
| --- | --- | --- | --- | --- | --- |
| AWS | Terraform, `deploy/aws-ecs` | ECS on Fargate behind an Application Load Balancer | RDS PostgreSQL, IAM authentication | AWS KMS | S3 |
| GCP | Terraform, `deploy/gcp-gke` | GKE Autopilot with the chart | Cloud SQL PostgreSQL, IAM authentication | Cloud KMS (HSM) | GCS |
| Azure | Bicep, `deploy/azure-container-apps` | Container Apps | PostgreSQL Flexible Server, Entra | Key Vault Premium | Blob Storage |

The structure follows tresor-server's recipes (its specs 012, 020 and 021):
- one root module per cloud, with a README and `examples/trial.tfvars`;
- CI that checks each recipe without a cloud;
- live scripts that build a stack, check it and delete it.

kista reuses tresor-server's design, not its code (tresor-server is BUSL).

This spec replaces spec 0013's phase 2 outline: the Container Apps Bicep becomes phase 3 here, and
the bootstrap from configuration and the Marketplace package become phase 4.

Delivery, each phase a pull request with its own reviews:

- **0. Server changes** the recipes need, with unit tests:
  - the configuration from an environment variable;
  - a CA for the store given inline;
  - top-level `aws` and `gcp` identities;
  - the `aws` and `gcp` database logins.
- **1. AWS**:
  - the ECS recipe;
  - two live runs: EKS through the chart with IRSA, and the recipe itself;
  - the docs page.
- **2. GCP**:
  - the GKE recipe, with chart changes for GKE;
  - its live run;
  - the docs page.

  Prerequisite: kista's first release (`v0.1.0`), so that the chart is published.
- **3. Azure**:
  - the Container Apps recipe;
  - its admin jobs;
  - its live run;
  - the docs page.
- **4. Bootstrap and the Marketplace**:
  - the bootstrap from configuration;
  - the Azure Marketplace managed application.

## Problem

- **Nothing has been proven against a real cloud.** The `awskms`, `gcpkms` and `azurekv` key
  sources and the S3, GCS and Azure Blob stores (specs 0004 and 0005) were tested against
  emulators.
  - GCP alone ran live (`deploy/gcp/up.sh` and `live.sh`), and then as the operator rather than
    as a workload.
  - Nothing ran on AWS.
  - moto cannot even produce a KMS signature that verifies: it hashes the digest again.
- **The database needs a password on AWS and GCP.** An operator keeps it in a Secret. Spec 0003
  and the platform's rule say the opposite: where the platform has an identity, there is no
  static secret. Today only Azure has an alternative (`store.login.kind: entra`).
- **Container platforms cannot give kista its configuration.** kista reads its configuration from a
  file (`-config`). The keys that matter are file-only, so `KISTA_*` variables cannot set them:
  `signers`, `blob`, `serve.listeners`, `auth`, `egress`. Fargate cannot mount a file from a
  secret or a config map, and Container Apps mounts only secrets. The database CA has the same
  problem: it reaches pgx only as `sslrootcert`, a path.
- **Wiring by hand has traps** that the chart's validation cannot see from inside a cluster:
  - a KMS key with the right spec, purpose tag and permissions;
  - a private bucket;
  - a database that grants a workload identity what kista's migrations need;
  - a load balancer that forwards plain http, which DuckDB's bootstrap needs (spec 0006);
  - probes on the health port;
  - `trusted_proxies` set to the load balancer's addresses.
- **The platforms set limits.**
  - **Cloud Run** caps the container's HTTP/1 requests at 32 MiB. It also caps responses that are
    not streamed: kista's bodies are served with a `Content-Length`, so they count. Only h2c
    lifts both caps. Cloud Run redirects http to https, and DuckDB's built-in client follows no
    redirects. Its filesystem lives in memory.
  - **Container Apps** cuts a request after 240 seconds, uploads and downloads alike. A replica
    gets 1 to 8 GiB of ephemeral storage, depending on its CPU. Plain http is redirected unless
    `allowInsecure` is set.
  - **GKE Autopilot** gives a general-purpose pod at most 10 GiB of ephemeral storage.
  - **Fargate** mounts no file from a secret, and its bind volumes may come up owned by root.

## Design

### Phase 0: server changes

- **Configuration from the environment.**
  - `-config env:<NAME>` reads the YAML from the variable `NAME`, at most 1 MiB, for `serve`,
    `admin` and `check`. It is decoded exactly as a file is: known fields only, then the `KISTA_*`
    overrides.
  - The configuration is not a secret: secrets stay in their own files and identities (spec
    0013), so a variable is a fit carrier.
  - A container's environment is visible to whoever can describe the task or the app, so the docs
    warn against putting secrets in it. The config check already refuses a password in
    `store.dsn`.
- **An inline CA for PostgreSQL.**
  - `store.ca_pem` holds PEM certificates. It is public data, and its `sslrootcert` equivalent
    means no file is needed.
  - It sets the pool's root CAs with `verify-full` and cannot be combined with `sslrootcert` in the
    DSN.
  - `pgCheckTLS` treats it as a CA source.
- **Top-level identities** for what is not a key source or a blob domain:
  - `aws: {region, identity}`: `identity` takes the `aws.Credentials` kinds that sources
    already use: `pod_identity`, `irsa` with `role_arn`, `imds`, `ecs`, and `static` in profile
    `dev` only.
  - `gcp: {principal, credentials_file}`: the same fields and rules as a source's `GCPCreds`.
  - The chart's identity checks (`kista.validate`, the principal walk) cover them:
    - `gcp.principal` must equal `gcp.serviceAccount`;
    - `aws.identity.role_arn` must equal `aws.irsa.roleArn`.
- **The `aws` database login:** RDS IAM authentication.
  - A token login gets the connection's host, port and user from the store (`TokenLogin.PasswordFor`,
    called from pgx's `BeforeConnect`); `entra` and `password` keep `Password(ctx)`.
  - The token comes from aws-sdk-go-v2's `feature/rds/auth.BuildAuthToken`. It is signed locally
    for `rds-db:connect` with the top-level `aws` identity, and lives for 15 minutes.
  - A token is made per new connection. A connection outlives its token, and the pool's existing
    30-minute connection lifetime applies.
  - A DSN with fallback hosts is refused, since a token is bound to one host.
- **The `gcp` database login:** Cloud SQL IAM authentication.
  - The password is an access token of the top-level `gcp` principal, with the scope
    `https://www.googleapis.com/auth/sqlservice.login` only. A full `cloud-platform` token is
    never sent to a database server.
  - `gcp.TokenSource` therefore takes the scope as a parameter, where it is hard-coded today.
  - The DSN's user must be the principal without `.gserviceaccount.com`, which is Cloud SQL's name
    for an IAM service-account user. The config check enforces it.
- **Rules for both logins.** Both need PostgreSQL with `verify-full` and a CA, from `store.ca_pem`
  or `sslrootcert`. The config check refuses:
  - a password setting beside them, and beside `entra` too, which today accepts one silently;
  - `aws` without `aws.region` and `aws.identity`;
  - `gcp` without `gcp.principal`.
- **Cloud SQL's trailing dot.** Certificates issued under `GOOGLE_MANAGED_CAS_CA` name the
  instance as `….sql.goog.`; tresor-server's live run hit this.
  - Go's host name check compares a SAN that is not a valid pattern exactly, and pgx checks the
    DSN's host as written. A DSN host written with the trailing dot therefore verifies with no code
    change (phase 0's test proves it); the docs say to write it so.

### Common to the recipes

- **Layout.** Each Terraform recipe is a root module:
  - `main.tf`, `variables.tf`, `outputs.tf`, `versions.tf`, `README.md`, `examples/trial.tfvars`;
  - a committed `.terraform.lock.hcl`, as Terraform advises for root modules;
  - a `.gitignore` for `.terraform/` and the state.

  It is a recipe that a user copies and edits, so its provider blocks are inside it. OpenTofu
  works too.

  Version constraints:

  | Requirement | Version |
  | --- | --- |
  | Terraform | 1.5 or later |
  | `hashicorp/aws` | `>= 5.70, < 7` |
  | `hashicorp/google` | `>= 6.10, < 8` |
  | `hashicorp/helm` | `>= 3.0, < 4` |
  | `hashicorp/kubernetes` | `>= 2.35, < 3` |
- **Shared inputs.**

  | Input | |
  | --- | --- |
  | `prefix` | validated; names every resource |
  | `region` | |
  | the network | an existing one, or one the recipe makes |
  | `image` | |
  | `domain` | the public URL's host |
  | `server_issuers` | YAML in a string, the configuration's `auth.server_issuers` |
  | `server_admins` | YAML in a string, the configuration's `auth.server_admins` (principals) |
  | `max_body`, `max_ingests` | |
  | `desired_count` | |
  | the database's size | |
  | `deletion_protection` | default `true` |
  | `key_deletion_days` | 30 by default; live runs set 7 |

  `image` takes a version tag or a digest; the recipe passes the digest when one is given.
- **One configuration.** The configuration is a single `locals` document. The service and every
  command task receive it as `KISTA_CONFIG`, read through `-config env:KISTA_CONFIG`.
  - It declares the health listener `{addr: ":8081", scheme: health}`.
  - It declares the serving listener `:8443` as `https` with `behind_proxy`.
  - It sets `spool_dir` to the spool volume.

  The cloud's identity fills `aws`, `gcp` or `azure` and each source's and domain's identity, with
  the `account` fields kista checks (spec 0004: the AWS account, the expected bucket owner).
- **No live secret.**
  - The workload's identity signs (KMS), stores (the bucket) and logs into the database (IAM or
    Entra).
  - Where a cloud insists on a database master password, it is used once by an initialisation
    task (below). On AWS, RDS manages it in Secrets Manager and it never enters Terraform's state.
    On GCP it reaches the state, and the initialisation task then changes it to a value nobody
    records.
- **The network.**
  - The database and the bucket are private. The service has no public address of its own, and the
    load balancer is the only public endpoint.
  - The load balancer listens on port 80 and port 443 and forwards both to the serving listener,
    without redirecting. DuckDB's bootstrap and passthrough autoload need plain http (spec 0006),
    and kista's downgrade rule applies to the forwarded scheme.
  - `trusted_proxies` names the load balancer's addresses, which differ per cloud (below). Anything
    else that reaches those addresses is kept out by the network's own rules: security groups, the
    chart's NetworkPolicy, or a VNet environment.
- **Probes.** Health checks go to `:8081/readyz` on every platform.
- **Spool sizing.** The spool needs (2 × `max_ingests` + 1) × `max_body` (spec 0013). Each recipe
  validates its inputs against its platform's ephemeral disk, leaving room for the image:
  - Fargate: `ephemeral_storage` of 21 to 200 GiB;
  - GKE Autopilot: 10 GiB per pod, so the recipe defaults to `max_body` 768 MiB and `max_ingests` 3
    (5.25 GiB);
  - Container Apps: by CPU, so 1 vCPU (4 GiB) defaults to `max_body` 512 MiB and `max_ingests` 2
    (2.5 GiB).
- **Idle timeouts.** kista closes idle keep-alive connections after 120 seconds. A load balancer's
  idle timeout must be shorter, or it reuses connections that kista has already closed and answers
  `502`. The ALB keeps its default of 60 seconds. GCP's backend timeout is a different setting:
  a response deadline (below).
- **First administration**, before phase 4's bootstrap:
  - `auth.server_admins` from the configuration names the operator's principal;
  - the management API and the console then create the tenant, channel, key, issuer and grants;
  - the admin commands run as the platform's one-off tasks: `aws ecs run-task` with overrides,
    `kubectl exec`, or Container Apps jobs.
- **CI**, with no cloud and no credentials, in `ci.yml` beside the `chart` job, with binaries pinned
  by SHA-256 as Helm's are:
  - `terraform`: Terraform 1.12, then `fmt -check -recursive`, `init -backend=false`, `validate`,
    and `terraform test` with `mock_provider` for the variable validations;
  - `bicep`: `az bicep build`, warnings treated as errors.

  Mock providers need Terraform 1.7, but users still need only 1.5: the tests are CI's.
- **Live runs.** `deploy/<cloud>/recipe-live.sh up | check | down | leftovers` drives a live run.
  The existing `deploy/gcp/live.sh` keeps its job and its interface: the gcpkms and gcs live tests,
  used by `gcp-live.yml` and `development.md`.
  - **Tagging.** Every resource the run makes carries the tag or label `purpose=kista-live`.
  - **`check`** runs what the kind test checks, through the cloud's load balancer:
    - signing and `.well-known`;
    - `blob check` and `blob verify`;
    - the database login;
    - DuckDB's `TestRemote`: the http bootstrap, a private install with a token, a refusal without
      one;
    - a rolling restart with `/readyz` polled throughout;
    - the client address in the audit is the caller's, not the load balancer's.
  - **`down`** deletes everything the run made.
  - **`leftovers`** lists anything that remains, by tag and by name prefix, because tagging APIs
    do not list every resource type (IAM on AWS, for one). It must print nothing after `down`,
    except what a cloud keeps for a fixed period, which it lists apart with the date it goes:
    - KMS keys pending deletion (7 days at least);
    - a GCP project pending deletion (30 days);
    - a soft-deleted Key Vault, when purge protection was on.
  - **Approval.** The owner approves each run before it starts, and a run is created and deleted on
    the same day.

### Phase 1: AWS

**The recipe** (`deploy/aws-ecs`) makes:

- **The task role** `<prefix>-task`, with no key. It may:
  - call `kms:DescribeKey`, `kms:ListResourceTags`, `kms:GetPublicKey` and `kms:Sign` on keys in its
    account and region whose tag `kista-purpose` is `channel-signing` or `attestation-signing`
    (condition `aws:ResourceTag/kista-purpose`), so a key added later with the tag is usable without
    a policy change;
  - call `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` and `s3:AbortMultipartUpload` on the
    bucket's objects, and `s3:ListBucket` and `s3:ListBucketMultipartUploads` on the bucket;
  - with SSE-KMS, call `kms:GenerateDataKey` and `kms:Decrypt` on that key;
  - call `rds-db:connect` on `arn:aws:rds-db:<region>:<account>:dbuser:<DbiResourceId>/kista`.

  The README notes that anyone allowed `kms:TagResource` can make a key usable by kista. In
  production an SCP or a permissions boundary should deny tagging `kista-purpose`.
- **The execution roles.** The service's pulls the image and writes logs. `db-init` has an
  execution role of its own, and only that role may read the master password's secret.
- **KMS.** One channel key: `RSA_2048`, `SIGN_VERIFY`, tagged `kista-purpose=channel-signing`, with
  `key_deletion_days`. Further keys are made with `aws kms create-key`, as the docs show.
- **S3.**
  - A bucket with public access blocked and `BucketOwnerEnforced`.
  - SSE-S3, or SSE-KMS if asked.
  - No versioning: bodies are content-addressed.
  - A lifecycle rule `AbortIncompleteMultipartUpload` after 7 days, the backstop spec 0005 promises.
- **RDS.**
  - PostgreSQL 17 with IAM authentication, in private subnets, not publicly accessible, encrypted,
    with backups.
  - `ca_cert_identifier` is pinned, so the recipe's CA bundle matches it.
  - The master password is managed by RDS in Secrets Manager.
- **`db-init`.** A one-time task with the image `postgres:17-alpine` (pinned by digest) and `psql`,
  the master password passed through ECS `secrets`.
  - It runs:

    ```sql
    CREATE ROLE kista LOGIN;
    GRANT rds_iam TO kista;
    GRANT kista TO <master>;
    ALTER DATABASE kista OWNER TO kista;
    ```

    and makes `kista` the owner of schema `public`, so migrations can create tables on PostgreSQL
    15 and later.
  - `terraform_data` with `local-exec` runs it through `aws ecs run-task`, waits for it to stop,
    checks its exit code, and retries while IAM propagates.
- **The CA.** The region's RDS CA bundle is a file in the recipe, with its checksum, and goes into
  the configuration as `store.ca_pem`: no download at apply time, and no file in the task.
- **ECS.**
  - A cluster, and the service on Fargate in private subnets with no public IP.
  - `ephemeral_storage` sized for the spool.
  - `readonlyRootFilesystem`, with a task volume at `/var/lib/kista/tmp` (TMPDIR and the spool).
  - `wait_for_steady_state` and the deployment circuit breaker.
  - A `health_check_grace_period_seconds` covering startup.
  - The live run confirms that the volume is writable by uid 65532. If Fargate mounts it root-owned,
    the image declares it a `VOLUME`, which Fargate creates with the image's ownership.
- **The load balancer.**
  - An ALB with the TLS policy `ELBSecurityPolicy-TLS13-1-2-2021-06` and an ACM certificate,
    DNS-validated in `route53_zone_id`, or a given `certificate_arn`.
  - Listeners on 80 and 443 forward to one target group on `:8443`, over `HTTP1`, with the health
    check on port 8081 at `/readyz`. The idle timeout stays at 60 seconds.
  - The task security group admits the ALB's security group on 8443 and 8081, and nothing else.
  - `trusted_proxies` is the CIDRs of the ALB's subnets: ALB nodes change addresses within them.
- **Commands.** A task definition `<prefix>-command` with the same configuration, run with
  `aws ecs run-task --overrides`. The README lists `migrate`, `blob check` and `blob gc -apply`.
- **The network.**
  - A VPC over two AZs with one NAT gateway, unless `vpc_id` names one. One NAT gateway suits a
    trial; production uses one per AZ.
  - Optional VPC endpoints for KMS, S3, Secrets Manager and Logs.
  - NAT is needed in any case: kista pulls from ghcr.io and reaches its issuers and upstreams.

**Live runs** (`deploy/aws/recipe-live.sh`, eu-central-1, an IAM user or SSO role made for the run):

1. **EKS through the chart.**
   - eksctl makes a cluster with OIDC and two nodes, and the AWS Load Balancer Controller with
     IRSA.
   - The script makes the KMS key, bucket and RDS instance in eksctl's VPC, all tagged.
   - kista's service account uses IRSA.
   - `check` runs, with the configuration from a ConfigMap as the chart does.
2. **The ECS recipe**, applied whole with `deletion_protection=false`, `skip_final_snapshot` and
   `key_deletion_days=7`, at `kista.aws.hugr-lab.com` in the delegated zone. Then `check`.
3. **`down`, in order:**
   - uninstall the chart, and wait until the controller has removed its ALB, target groups and
     security groups (otherwise the VPC cannot go);
   - `eksctl delete cluster --wait`, which also removes its CloudFormation stacks and the OIDC
     provider;
   - `terraform destroy`;
   - empty and delete the bucket;
   - schedule the KMS keys for deletion and remove their aliases;
   - delete the log groups (ECS, RDS exports, EKS control plane), the IAM roles and the policies.
4. **`leftovers`** checks, by tag and by prefix:
   - the tagging API;
   - IAM;
   - `elbv2`;
   - RDS instances and snapshots;
   - `logs`;
   - CloudFormation;
   - Elastic IPs;
   - ENIs.

   The owner deletes the run's IAM user afterwards.

tresor-server's run reported account obstacles, which the docs page lists as observations to check
first:
- an organisation's SCP that allowed only Bedrock;
- Identity Center sourced from Builder ID, which could not assign accounts;
- no console sign-in for IAM users on the Free plan.

### Phase 2: GCP

**GKE Autopilot, not Cloud Run.**
- Publishing uploads uncompressed binaries, often over 32 MiB. Downloads carry a `Content-Length`
  and are capped as well.
- Cloud Run redirects http, which DuckDB's bootstrap cannot follow.
- The spool would cost memory.
- Working around all of this needs h2c in kista and an external load balancer.

GKE with the chart avoids it, and spec 0013 tried its parts (`gke.sh`).

**Chart changes**, made in this phase:
- `gke.backendConfig.timeoutSec`, default 600. The backend service's default is 30 seconds for the
  whole response, which cuts slow publishes and downloads.
- `gke.managedCertificate`, a `ManagedCertificate` for the Ingress hosts. Rendering it in the chart
  avoids `kubernetes_manifest`, which needs a live API server at plan time.
- The Ingress annotations for a global static IP and the certificate, through the existing
  `ingress.annotations`.

**The recipe** (`deploy/gcp-gke`, providers `google`, `helm` and `kubernetes`) makes:

- **The cluster.** GKE Autopilot with Workload Identity, in a VPC of its own or a given one.
- **The identity.** A service account `<prefix>-kista`, bound to kista's Kubernetes service account
  through Workload Identity. It holds:
  - `roles/cloudkms.signerVerifier` and `roles/cloudkms.viewer` on the key ring;
  - `roles/storage.objectUser` on the bucket (spec 0005);
  - `roles/cloudsql.instanceUser` on the project, with a condition naming the instance.

  kista connects to the private IP directly, so no `cloudsql.client` role is needed.
- **Cloud KMS.** A key ring and a channel key: `RSA_SIGN_PKCS1_2048_SHA256`, protection level `HSM`,
  label `kista-purpose=channel-signing`.
- **GCS.** A bucket with uniform access, public access prevented, and no versioning.
- **Cloud SQL.**
  - PostgreSQL 17 on a private IP, through private services access.
  - `server_ca_mode = "GOOGLE_MANAGED_CAS_CA"`, so the certificate names the instance's DNS name,
    and a private DNS zone that serves the name.
  - `ssl_mode = "ENCRYPTED_ONLY"`, IAM authentication, and backups.
  - The CA comes from the instance's `server_ca_cert` and goes into `store.ca_pem`.
  - The IAM user is `<prefix>-kista@<project>.iam`, of type `CLOUD_IAM_SERVICE_ACCOUNT`.
- **`db-init`.** A one-time Kubernetes Job with `postgres:17-alpine` (pinned by digest) and the
  built-in `postgres` user's password.
  - That password is generated by Terraform, so it is in the state. The Job uses it once:
    - it grants the IAM user its database (`GRANT "<u>" TO postgres; ALTER DATABASE kista OWNER TO
      "<u>"` and ownership of `public`);
    - then it sets the built-in user's password to a random value that is printed nowhere.

    The state keeps a dead password.
  - The users and the database are `deletion_policy = "ABANDON"`, because the IAM user owns
    objects that Terraform cannot drop.
  - Terraform waits for the Job to complete.
- **The chart.**
  - Installed with `helm_release` from `oci://ghcr.io/hugr-lab/charts`, at a pinned version.
  - Values:
    - `gcp.serviceAccount`;
    - the Ingress (class `gce`), with the managed certificate and the static IP;
    - `gke.backendConfig`;
    - the NetworkPolicy's `loadBalancerRanges`;
    - the configuration with `store.login.kind: gcp`;
    - the spool sizes.
  - The live run may install a local chart path and an `:sha-<commit>` digest before the first
    release.
- **The address.**
  - A global static IP and a Google-managed certificate for `domain`. DNS must point at the IP
    before the certificate is issued, which can take tens of minutes; the README says so.
  - The GKE Ingress serves http and https; no redirect is configured.
- **`trusted_proxies`.** Google's front ends (`35.191.0.0/16`, `130.211.0.0/22`) and the static
  IP's `/32`. The external load balancer appends its own address to `X-Forwarded-For`, and kista
  takes the rightmost untrusted hop, so without the `/32` every client would appear as the load
  balancer. `deploy/gcp/gke.sh` has the same gap; it is removed, and this run replaces it.

**Live run** (`deploy/gcp/recipe-live.sh`):
- The recipe runs in a project of its own, under `gcp.hugr-lab.com` or the domain the owner gives.
- `check` runs through the load balancer.
- `down` deletes the project, the only clean way out for Cloud KMS key rings (spec 0004).
- `leftovers` confirms the project is `DELETE_REQUESTED`.

### Phase 3: Azure

**The recipe** (`deploy/azure-container-apps`, `main.bicep` with modules) makes:

- **The kista identity.** A user-assigned managed identity.
- **The network.** A VNet with an infrastructure subnet for the Container Apps environment. This
  is what gives `trusted_proxies` a range of the operator's own: the environment's Envoy reaches
  kista from that subnet. The live run confirms which address kista sees.
- **Key Vault Premium.**
  - `enableRbacAuthorization`, and `purge_protection` as a parameter (on by default; the live run
    turns it off and uses a name unique to the run).
  - One channel key: `RSA-HSM` 2048, `keyOps: ['sign', 'verify']` (azurekv refuses any other
    operation), not exportable, with the tag `kista-purpose=channel-signing`.
  - A custom role with the data actions `Microsoft.KeyVault/vaults/keys/read` and
    `…/keys/sign/action`, assigned to the identity on the vault. Creating a role definition needs
    Owner or User Access Administrator, which phase 4's managed application has to account for.
  - The live run confirms that tags set through ARM are the key tags azurekv reads.
- **PostgreSQL Flexible Server.**
  - Entra authentication only, `require_secure_transport`, and `verify-full` against the server's
    CA in `store.ca_pem`.
  - **The trade-off.** The kista identity is the server's Entra administrator, so it is a member of
    `azure_pg_admin` on its own server. Bicep cannot create a narrower Entra role without a SQL
    step. The server is dedicated to kista, and the docs state the trade-off.
  - The alternative a production deployment may take: an Entra group as the administrator, and a
    one-off `pgaadauth_create_principal` for the kista identity.
- **Azure SQL** is the second module, Entra only, for organisations on SQL Server.
- **Blob Storage.** A Storage account with `allowBlobPublicAccess = false` and shared keys
  disabled, a private container, and `Storage Blob Data Contributor` on the container for the
  identity.
- **Log Analytics.**
- **The environment and the app.**
  - External ingress with `allowInsecure: true`, so http is served without a redirect.
  - Probes on `:8081` (Container Apps allows a probe port other than the ingress port).
  - An `EmptyDir` volume at `/var/lib/kista/tmp`, with `TMPDIR` pointing there.
  - Spool sizes validated against the CPU chosen.
  - The configuration in `KISTA_CONFIG`.
- **Admin jobs.** One manual Container Apps job per command:
  - `migrate`;
  - `blob check`;
  - `blob verify`;
  - `blob gc -apply`, with the `-force` decision left to `gc.interval`.

  Each command is its own job because `az containerapp job start --args` replaces the template,
  environment included; tresor-server found this.
- **The 240-second limit.** Container Apps cuts any request after 240 seconds, upload or download.
  The docs say what that means for large files on slow links, and recommend publishing from a
  runner in the same region.

**Live run** (`deploy/azure/recipe-live.sh`):
- The recipe runs in a resource group of its own, with purge protection off.
- The first administration is done through the management API as the configured server admin.
- `check` runs.
- `down` deletes the group and purges the vault.
- `leftovers` checks the subscription for the run's tag and for soft-deleted vaults.

### Phase 4: bootstrap and the Marketplace

This is spec 0013's outline, less the Bicep, which moved to phase 3:

- **The `bootstrap` section** creates, once and idempotently, with events:
  - the first tenant;
  - its channel;
  - its versions;
  - its keys;
  - its issuer;
  - its admin grant.
- **The Marketplace package** is built from phase 3's Bicep. It needs role-definition rights in the
  managed resource group.

An addendum details this phase before its code.

### Docs

- **New pages** under Deployment, in `website/sidebars.ts` and linked from `deployment/index.md`:
  `deployment/aws.md`, `deployment/gcp.md`, `deployment/azure.md`. Each covers:
  - what the recipe makes;
  - deploying it;
  - running a command;
  - the network;
  - the limits;
  - production;
  - destroying it.
- **Updated pages:**
  - `configuration.md`: `-config env:`, `store.ca_pem`, the `aws` and `gcp` sections, and the new
    logins;
  - `security.md`: workload identities, no live secret in Terraform's state, and the tag-condition
    advice;
  - `development.md`: the recipe live runs, their approval, `down` and `leftovers`.
- **Each recipe's README** is a short version of its page.

### Changes to earlier specs

- **0013**:
  - its status line and summary;
  - its phase 2 outline now points here, and phase 2 no longer describes Azure;
  - the Azure key set is one channel key for the recipe, plus attestation keys as needed;
  - `deploy/azure` is now `deploy/azure-container-apps`.
- **0005**: the S3 permissions gain `s3:ListBucketMultipartUploads` and `s3:AbortMultipartUpload`.
- **0003**: the logins `aws` and `gcp`, and `store.ca_pem`.
- **0001**: the follow-up row for 0013.
- **specs/README.md**: the rows for 0013 and 0020.

## Security

- **No live static credential.**
  - KMS, the bucket and the database are reached with the workload's identity.
  - RDS's master password stays in Secrets Manager, readable only by `db-init`'s execution role.
  - GCP's built-in password is changed by `db-init` after its single use.
  - Terraform's state holds no working secret.
- **Narrow tokens.**
  - The database receives a `sqlservice.login` token (GCP) or an RDS token for one host and user
    (AWS), never a general cloud token.
  - Tokens are made per connection and never logged.
- **Least privilege.**
  - KMS: AWS keys by purpose tag, GCP by key ring, Azure by a custom role on one vault.
  - Storage: one bucket or container, with only the actions kista calls.
  - Database: one user. The Azure trade-off above is the exception, and it is stated.
  - Spec 0004's checks at open still apply: purpose tag, key spec, HSM, operations.
- **The configuration in the environment** holds no secret. The config check refuses secrets in
  the places it can see (`store.dsn`). The docs say who can read a task's or an app's environment.
- **Private by default.** Database and bucket have no public access, and the service is reached
  only through the load balancer.
  - `trusted_proxies` names the load balancer's ranges.
  - The network rules admit only the load balancer: security groups, the NetworkPolicy, the VNet.
  - The live run checks that the audit records the caller's address.
- **The CA inline.** `store.ca_pem` replaces a file, not a check: `verify-full` still applies, and
  pgx's system roots are not added beside it.
- **Live runs.**
  - Each run has an identity of its own (an IAM user or role, a project, a resource group), removed
    afterwards.
  - Its resources are tagged `purpose=kista-live`.
  - Nothing secret is printed.
  - `leftovers` proves the cleanup.

## Testing

- **Phase 0 (Go):**
  - `-config env:`: the size cap, known fields, the overrides after it, a missing variable;
  - `store.ca_pem` with `verify-full`, combined with `sslrootcert` (refused), and an invalid PEM;
  - the `aws` login: `BuildAuthToken` with a fixed clock and static test credentials, the
    connection's host, port and user, and a multi-host DSN refused;
  - the `gcp` login: a fake metadata server asked for `sqlservice.login` only, and the DSN user
    rule;
  - config validation of the new sections and kinds, including a password beside `entra` now
    refused;
  - the trailing-dot SAN against a test certificate, with and without code;
  - the chart's checks on the new identities.
- **CI:**
  - `terraform fmt`, `validate` and `terraform test` with mock providers;
  - `az bicep build`.
- **Live**, once per phase: `check`, then `down`, then `leftovers` empty but for timed retentions.

## Alternatives considered

- **Cloud Run for GCP**, as tresor-server uses: rejected for its HTTP/1 caps, its redirect and its
  in-memory disk (Problem). GKE costs a cluster, and Autopilot keeps that cost per pod.
- **EKS with Terraform for AWS.** EKS is covered by the chart and the live run. ECS is what an AWS
  user without Kubernetes reaches for: no request cap, up to 200 GiB of ephemeral disk, and an ALB
  that forwards http.
- **An init container writing the configuration file.** It needs an image with a shell (kista's
  is distroless), pinned and maintained besides kista. `-config env:` is a few lines in kista and
  serves Fargate and Container Apps alike.
- **A database password in the cloud's secret manager.** It is a static secret where the platform
  has an identity. `password_file` and `password_env` remain for those who want it.
- **One reusable Terraform module per cloud.** Users edit recipes, and a module's interface would
  freeze choices that belong to them.

## Follow-ups

- **Cloud Run**, once kista serves h2c and a recipe adds an external load balancer, if users ask.
- **More live coverage**: EKS Pod Identity and instance roles; Azure SQL with Entra.
- **Bundle keys** (spec 0014) in AWS KMS and Cloud KMS.
- **A generic ephemeral volume** in the chart, for spools larger than Autopilot's 10 GiB.
