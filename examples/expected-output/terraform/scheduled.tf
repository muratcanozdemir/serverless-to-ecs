
# ── ECS Scheduled Tasks ────────────────────────────────────
# These replace EventBridge scheduled rules → Lambda with
# EventBridge → ECS RunTask. Same scheduling, containerized execution.

resource "aws_cloudwatch_event_rule" "ecommerce_stale_order_cleanup" {
  name                = "${local.project}-ecommerce-stale-order-cleanup"
  schedule_expression = "rate(4 hours)"
  description         = "Migrated from Lambda: ProcessOrderFn"
  tags                = local.tags
}


resource "aws_cloudwatch_event_target" "ecommerce_stale_order_cleanup" {
  rule     = aws_cloudwatch_event_rule.ecommerce_stale_order_cleanup.name
  arn      = aws_ecs_cluster.main.arn
  role_arn = aws_iam_role.ecs_events.arn

  ecs_target {
    task_count          = 1
    task_definition_arn = aws_ecs_task_definition.orderqueue_processor.arn
    launch_type         = "FARGATE"

    network_configuration {
      subnets         = var.subnet_ids
      security_groups = [aws_security_group.ecs.id]
    }
  }
}



