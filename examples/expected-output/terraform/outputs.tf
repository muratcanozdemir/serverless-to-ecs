output "ecs_cluster_arn" {
  value = aws_ecs_cluster.main.arn
}

output "alb_dns_name" {
  description = "Point DNS or CloudFront here to start the strangler migration"
  value       = aws_lb.main.dns_name
}

output "alb_zone_id" {
  description = "Route53 alias target zone ID"
  value       = aws_lb.main.zone_id
}


output "orderapi_svc_task_definition" {
  value = aws_ecs_task_definition.orderapi_svc.arn
}

output "orderqueue_processor_task_definition" {
  value = aws_ecs_task_definition.orderqueue_processor.arn
}

output "scheduled_tasks_task_definition" {
  value = aws_ecs_task_definition.scheduled_tasks.arn
}

output "ecommerce_order_workflow_worker_task_definition" {
  value = aws_ecs_task_definition.ecommerce_order_workflow_worker.arn
}

