
# ┌─────────────────────────────────────────────────────────┐
# │  Strangler pattern: ALB routes traffic to ECS services  │
# │  while API Gateway continues to serve unmigrated routes │
# │                                                         │
# │  Migration steps:                                       │
# │  1. Deploy ECS services with desired_count = 0          │
# │  2. Scale up one service at a time                      │
# │  3. Point DNS/CloudFront to this ALB                    │
# │  4. Shift traffic route by route                        │
# │  5. Decommission Lambda functions as routes migrate     │
# └─────────────────────────────────────────────────────────┘

resource "aws_lb" "main" {
  name               = "${local.project}-alb"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = var.subnet_ids

  tags = local.tags
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "fixed-response"

    fixed_response {
      content_type = "text/plain"
      message_body = "route not migrated"
      status_code  = "404"
    }
  }
}

resource "aws_lb_target_group" "orderapi_svc" {
  name        = "orderapi-svc"
  port        = 8080
  protocol    = "HTTP"
  vpc_id      = var.vpc_id
  target_type = "ip"

  health_check {
    path                = "/health"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    interval            = 30
    timeout             = 5
  }

  tags = local.tags
}

resource "aws_lb_listener_rule" "orderapi_svc" {
  listener_arn = aws_lb_listener.http.arn
  priority     = 100

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.orderapi_svc.arn
  }

  condition {
    path_pattern {
      values = ["/orders/{orderId}", "/orders/{orderId}/*", "/orders", "/orders/*"]
    }
  }
}


