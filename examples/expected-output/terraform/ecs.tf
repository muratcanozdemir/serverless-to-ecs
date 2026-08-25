resource "aws_ecs_cluster" "main" {
  name = "${local.project}-cluster"

  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = local.tags
}

resource "aws_cloudwatch_log_group" "ecs" {
  name              = "/ecs/${local.project}"
  retention_in_days = 14
  tags              = local.tags
}

# ── orderapi-svc (api) ──
# Source Lambdas: CreateOrderFn
# Reason: shared API Gateway: OrderApi

resource "aws_ecs_task_definition" "orderapi_svc" {
  family                   = "orderapi-svc"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  container_definitions = jsonencode([{
    name  = "orderapi-svc"
    image = var.orderapi_svc_image

    portMappings = [{
      containerPort = 8080
      protocol      = "tcp"
    }]

    environment = [
      { name = "ORDER_QUEUE", value = "TODO_OrderQueue" },
      { name = "ORDER_TABLE", value = "TODO_OrderTable" },
      { name = "STAGE", value = "production" },
    ]
    secrets = [
    ]
    mountPoints = [
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.ecs.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = "orderapi-svc"
      }
    }
  }])

  tags = local.tags
}

resource "aws_ecs_service" "orderapi_svc" {
  name            = "orderapi-svc"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.orderapi_svc.arn
  desired_count   = var.orderapi_svc_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets         = var.subnet_ids
    security_groups = [aws_security_group.ecs.id]
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.orderapi_svc.arn
    container_name   = "orderapi-svc"
    container_port   = 8080
  }

  depends_on = [aws_lb_listener_rule.orderapi_svc]

  tags = local.tags
}


# ── orderqueue-processor (queue-processor) ──
# Source Lambdas: ProcessOrderFn
# Reason: SQS trigger: OrderQueue

resource "aws_ecs_task_definition" "orderqueue_processor" {
  family                   = "orderqueue-processor"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  container_definitions = jsonencode([{
    name  = "orderqueue-processor"
    image = var.orderqueue_processor_image

    portMappings = [{
      containerPort = 8080
      protocol      = "tcp"
    }]

    environment = [
      { name = "ORDER_TABLE", value = "TODO_OrderTable" },
      { name = "STAGE", value = "production" },
    ]
    secrets = [
    ]
    mountPoints = [
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.ecs.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = "orderqueue-processor"
      }
    }
  }])

  tags = local.tags
}

resource "aws_ecs_service" "orderqueue_processor" {
  name            = "orderqueue-processor"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.orderqueue_processor.arn
  desired_count   = var.orderqueue_processor_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets         = var.subnet_ids
    security_groups = [aws_security_group.ecs.id]
  }

  tags = local.tags
}


# ── scheduled-tasks (scheduled) ──
# Source Lambdas: DailyReportFn
# Reason: EventBridge schedule triggers

resource "aws_ecs_task_definition" "scheduled_tasks" {
  family                   = "scheduled-tasks"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 512
  memory                   = 1024
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  container_definitions = jsonencode([{
    name  = "scheduled-tasks"
    image = var.scheduled_tasks_image

    portMappings = [{
      containerPort = 8080
      protocol      = "tcp"
    }]

    environment = [
      { name = "ORDER_TABLE", value = "TODO_OrderTable" },
      { name = "STAGE", value = "production" },
    ]
    secrets = [
    ]
    mountPoints = [
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.ecs.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = "scheduled-tasks"
      }
    }
  }])

  tags = local.tags
}


# ── ecommerce-order-workflow-worker (orchestrated) ──
# Source Lambdas: InventoryCheckerFn, FulfillOrderFn, NotifyCustomerFn
# Reason: Step Functions orchestration: OrderWorkflow

resource "aws_ecs_task_definition" "ecommerce_order_workflow_worker" {
  family                   = "ecommerce-order-workflow-worker"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  container_definitions = jsonencode([{
    name  = "ecommerce-order-workflow-worker"
    image = var.ecommerce_order_workflow_worker_image

    portMappings = [{
      containerPort = 8080
      protocol      = "tcp"
    }]

    environment = [
      { name = "STAGE", value = "production" },
      { name = "ORDER_TABLE", value = "TODO_OrderTable" },
      { name = "NOTIFICATION_TOPIC", value = "TODO_NotificationTopic" },
    ]
    secrets = [
    ]
    mountPoints = [
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.ecs.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = "ecommerce-order-workflow-worker"
      }
    }
  }])

  tags = local.tags
}

resource "aws_ecs_service" "ecommerce_order_workflow_worker" {
  name            = "ecommerce-order-workflow-worker"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.ecommerce_order_workflow_worker.arn
  desired_count   = var.ecommerce_order_workflow_worker_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets         = var.subnet_ids
    security_groups = [aws_security_group.ecs.id]
  }

  tags = local.tags
}


