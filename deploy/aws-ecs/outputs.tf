output "url" {
  description = "kista's public URL (serve.public_url)."
  value       = "https://${var.domain}"
}

output "lb_dns_name" {
  description = "The load balancer's name: point the domain at it when the recipe has no route53_zone_id."
  value       = aws_lb.lb.dns_name
}

output "task_role_arn" {
  value = aws_iam_role.task.arn
}

output "channel_signer" {
  description = "The channel key as a signer reference, for `kista admin key add <tenant>/<channel> -signer …`."
  value       = "aws:${aws_kms_key.channel.key_id}"
}

output "bucket" {
  value = aws_s3_bucket.bodies.bucket
}

output "db_endpoint" {
  value = aws_db_instance.db.address
}

output "cluster" {
  value = aws_ecs_cluster.cluster.name
}

output "command_task_definition" {
  value = aws_ecs_task_definition.command.family
}

output "private_subnet_ids" {
  value = local.private_subnet_ids
}

output "task_security_group" {
  value = aws_security_group.task.id
}

output "log_group" {
  value = aws_cloudwatch_log_group.logs.name
}
