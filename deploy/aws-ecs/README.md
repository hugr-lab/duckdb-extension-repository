# kista on ECS Fargate (Terraform)

A recipe (spec 0020): kista on ECS Fargate behind an Application Load Balancer, its task role the
only identity (no key), signing with an AWS KMS key, bodies in S3, the store on RDS PostgreSQL by
IAM authentication over verify-full. Copy it and edit it to your needs; OpenTofu works too.

```sh
cp examples/trial.tfvars my.tfvars   # domain, route53_zone_id, image, server_issuers, server_admins
terraform init
terraform apply -var-file my.tfvars
```

What it makes, the first administration, commands, the network, the limits, production and
destroying it: the docs' [AWS page](../../website/docs/deployment/aws.md).
