variable "region" {
  type    = string
  default = "eu-central-1"
}

variable "project_name" {
  type    = string
  default = "synthetic"
}

variable "vpc_id" {
  type        = string
  description = "VPC ID for ECS services and ALB"
}

variable "subnet_ids" {
  type        = list(string)
  description = "Subnet IDs for ECS tasks and ALB (use public subnets if no NAT)"
}

variable "tags" {
  type    = map(string)
  default = {}
}

# ── Container images ────────────────────────────────────────
# Replace these with your actual ECR image URIs after
# containerizing the Lambda function code.

variable "orderapi_svc_image" {
  type        = string
  default     = "PLACEHOLDER:latest"
  description = "Container image for orderapi-svc (api)"
}

variable "orderqueue_processor_image" {
  type        = string
  default     = "PLACEHOLDER:latest"
  description = "Container image for orderqueue-processor (queue-processor)"
}

variable "scheduled_tasks_image" {
  type        = string
  default     = "PLACEHOLDER:latest"
  description = "Container image for scheduled-tasks (scheduled)"
}

variable "ecommerce_order_workflow_worker_image" {
  type        = string
  default     = "PLACEHOLDER:latest"
  description = "Container image for ecommerce-order-workflow-worker (orchestrated)"
}

# ── Service scaling ─────────────────────────────────────────
# Set desired_count to 0 to keep a service defined but dormant.
# This is the strangler toggle: bring services up one at a time,
# shift traffic, then decommission the Lambda equivalent.

variable "orderapi_svc_desired_count" {
  type    = number
  default = 2
}

variable "orderqueue_processor_desired_count" {
  type    = number
  default = 1
}

variable "scheduled_tasks_desired_count" {
  type    = number
  default = 0
}

variable "ecommerce_order_workflow_worker_desired_count" {
  type    = number
  default = 1
}

# ── Secrets (Secrets Manager / SSM Parameter Store) ─────────
# Replace these defaults with the actual ARNs. The source column notes
# where the value came from in the original template.

# ── EFS (existing file systems — not created by this Terraform) ─

