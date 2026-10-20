terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.80" }
  }
}

provider "aws" {
  region                      = var.region
  access_key                  = var.use_floci ? "test" : null
  secret_key                  = var.use_floci ? "test" : null
  skip_credentials_validation = var.use_floci
  skip_metadata_api_check     = var.use_floci
  skip_requesting_account_id  = var.use_floci
  dynamic "endpoints" {
    for_each = var.use_floci ? [1] : []
    content {
      dynamodb   = "http://localhost:4566"
      ecr        = "http://localhost:4566"
      batch      = "http://localhost:4566"
      iam        = "http://localhost:4566"
      logs       = "http://localhost:4566"
      sts        = "http://localhost:4566"
      secretsmanager = "http://localhost:4566"
      scheduler  = "http://localhost:4566"
    }
  }
}

locals {
  image = var.image != "" ? var.image : "${aws_ecr_repository.app.repository_url}:latest"
  jobs  = {
    bill      = { command = ["bill"], schedule = var.billing_cron }
    retry     = { command = ["retry"], schedule = var.retry_cron }
    reconcile = { command = ["reconcile"], schedule = var.reconcile_cron }
  }
}

resource "aws_ecr_repository" "app" {
  name                 = var.name
  image_tag_mutability = "MUTABLE"
  force_delete         = var.use_floci
}

resource "aws_dynamodb_table" "subscriptions" {
  name         = "${var.name}-subscriptions"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "customer_id"
  attribute {
    name = "customer_id"
    type = "S"
  }
}

resource "aws_dynamodb_table" "charges" {
  name         = "${var.name}-charges"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "charge_id"
  attribute {
    name = "charge_id"
    type = "S"
  }
  point_in_time_recovery { enabled = true }
}

resource "aws_secretsmanager_secret" "stripe" {
  name                    = "${var.name}/stripe-api-key"
  recovery_window_in_days = var.use_floci ? 0 : 7
}

resource "aws_secretsmanager_secret_version" "stripe" {
  secret_id     = aws_secretsmanager_secret.stripe.id
  secret_string = var.stripe_secret_value
  lifecycle { ignore_changes = [secret_string] }
}

resource "aws_cloudwatch_log_group" "batch" {
  name              = "/aws/batch/${var.name}"
  retention_in_days = 30
}

# ---------- IAM ----------
data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# Execution role: pull image, write logs, inject the Stripe secret.
resource "aws_iam_role" "execution" {
  name               = "${var.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}
resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}
resource "aws_iam_role_policy" "execution_secret" {
  role = aws_iam_role.execution.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = aws_secretsmanager_secret.stripe.arn }]
  })
}

# Job role: what the application code itself may do.
resource "aws_iam_role" "job" {
  name               = "${var.name}-job"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}
resource "aws_iam_role_policy" "job" {
  role = aws_iam_role.job.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:Scan", "dynamodb:Query"]
      Resource = [aws_dynamodb_table.subscriptions.arn, aws_dynamodb_table.charges.arn]
    }]
  })
}

# ---------- AWS Batch on Fargate ----------
data "aws_iam_policy_document" "batch_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["batch.amazonaws.com"]
    }
  }
}
resource "aws_iam_role" "batch_service" {
  name               = "${var.name}-batch-service"
  assume_role_policy = data.aws_iam_policy_document.batch_assume.json
}
resource "aws_iam_role_policy_attachment" "batch_service" {
  role       = aws_iam_role.batch_service.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSBatchServiceRole"
}

resource "aws_batch_compute_environment" "fargate" {
  compute_environment_name = "${var.name}-fargate"
  type                     = "MANAGED"
  service_role             = aws_iam_role.batch_service.arn
  compute_resources {
    type               = "FARGATE"
    max_vcpus          = 4
    subnets            = var.subnet_ids
    security_group_ids = var.security_group_ids
  }
  depends_on = [aws_iam_role_policy_attachment.batch_service]
}

resource "aws_batch_job_queue" "main" {
  name     = "${var.name}-queue"
  state    = "ENABLED"
  priority = 1
  compute_environment_order {
    order               = 1
    compute_environment = aws_batch_compute_environment.fargate.arn
  }
}

resource "aws_batch_job_definition" "job" {
  for_each             = local.jobs
  name                 = "${var.name}-${each.key}"
  type                 = "container"
  platform_capabilities = ["FARGATE"]
  retry_strategy { attempts = 1 } # business-level retries live in the app; avoid blind re-runs of charges
  timeout { attempt_duration_seconds = 3600 }
  container_properties = jsonencode({
    image            = local.image
    command          = each.value.command
    executionRoleArn = aws_iam_role.execution.arn
    jobRoleArn       = aws_iam_role.job.arn
    resourceRequirements = [
      { type = "VCPU", value = "0.5" },
      { type = "MEMORY", value = "1024" },
    ]
    fargatePlatformConfiguration = { platformVersion = "LATEST" }
    networkConfiguration         = { assignPublicIp = "DISABLED" }
    environment = [
      { name = "SUBSCRIPTIONS_TABLE", value = aws_dynamodb_table.subscriptions.name },
      { name = "CHARGES_TABLE", value = aws_dynamodb_table.charges.name },
      { name = "STRIPE_API_BASE", value = var.stripe_api_base },
      { name = "MAX_ATTEMPTS", value = "4" },
    ]
    secrets = [{ name = "STRIPE_API_KEY", valueFrom = aws_secretsmanager_secret.stripe.arn }]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.batch.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = each.key
      }
    }
  })
}

# ---------- Scheduling (EventBridge Scheduler -> Batch SubmitJob) ----------
data "aws_iam_policy_document" "scheduler_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }
  }
}
resource "aws_iam_role" "scheduler" {
  name               = "${var.name}-scheduler"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}
resource "aws_iam_role_policy" "scheduler" {
  role = aws_iam_role.scheduler.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["batch:SubmitJob"], Resource = "*" }]
  })
}

resource "aws_scheduler_schedule" "job" {
  for_each            = local.jobs
  name                = "${var.name}-${each.key}"
  schedule_expression = each.value.schedule
  flexible_time_window { mode = "OFF" }
  target {
    arn      = "arn:aws:scheduler:::aws-sdk:batch:submitJob"
    role_arn = aws_iam_role.scheduler.arn
    input = jsonencode({
      JobName       = "${var.name}-${each.key}"
      JobQueue      = aws_batch_job_queue.main.arn
      JobDefinition = aws_batch_job_definition.job[each.key].arn
    })
  }
}

output "ecr_repository_url" { value = aws_ecr_repository.app.repository_url }
output "job_queue" { value = aws_batch_job_queue.main.name }
