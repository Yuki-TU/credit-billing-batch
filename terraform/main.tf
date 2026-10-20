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
      rds            = "http://localhost:4566"
      ecr            = "http://localhost:4566"
      batch          = "http://localhost:4566"
      iam            = "http://localhost:4566"
      logs           = "http://localhost:4566"
      sts            = "http://localhost:4566"
      secretsmanager = "http://localhost:4566"
      scheduler      = "http://localhost:4566"
    }
  }
}

locals {
  image = var.image != "" ? var.image : "${aws_ecr_repository.app.repository_url}:latest"
  jobs = {
    bill      = { command = ["bill"], schedule = var.billing_cron }
    retry     = { command = ["retry"], schedule = var.retry_cron }
    reconcile = { command = ["reconcile"], schedule = var.reconcile_cron }
    migrate   = { command = ["migrate"], schedule = null } # submit manually after each deploy
  }
  scheduled_jobs = { for k, v in local.jobs : k => v if v.schedule != null }
  # JSON secret holding {"password": ...}: RDS-managed in AWS, a plain secret on Floci.
  db_secret_arn = var.use_floci ? aws_secretsmanager_secret.db_local[0].arn : aws_rds_cluster.main.master_user_secret[0].secret_arn
}

resource "aws_ecr_repository" "app" {
  name                 = var.name
  image_tag_mutability = "MUTABLE"
  force_delete         = var.use_floci
}

# ---------- Aurora MySQL (Serverless v2) ----------
resource "aws_db_subnet_group" "db" {
  count      = var.use_floci ? 0 : 1
  name       = "${var.name}-db"
  subnet_ids = var.subnet_ids
}

resource "aws_security_group" "db" {
  count       = var.use_floci ? 0 : 1
  name        = "${var.name}-db"
  description = "Aurora MySQL: allow 3306 from the batch tasks"
  vpc_id      = var.vpc_id
  ingress {
    from_port       = 3306
    to_port         = 3306
    protocol        = "tcp"
    security_groups = var.security_group_ids
  }
}

resource "aws_rds_cluster" "main" {
  cluster_identifier = var.name
  engine             = "aurora-mysql"
  engine_mode        = "provisioned"
  engine_version     = var.db_engine_version
  database_name      = var.db_name
  master_username    = var.db_username
  # In AWS, RDS generates the password and keeps it in Secrets Manager.
  manage_master_user_password = var.use_floci ? null : true
  master_password             = var.use_floci ? var.local_db_password : null

  db_subnet_group_name      = var.use_floci ? null : aws_db_subnet_group.db[0].name
  vpc_security_group_ids    = var.use_floci ? null : [aws_security_group.db[0].id]
  storage_encrypted         = true
  backup_retention_period   = 7
  deletion_protection       = !var.use_floci
  skip_final_snapshot       = var.use_floci
  final_snapshot_identifier = var.use_floci ? null : "${var.name}-final"

  serverlessv2_scaling_configuration {
    min_capacity = var.db_min_acu
    max_capacity = var.db_max_acu
  }
}

resource "aws_rds_cluster_instance" "writer" {
  identifier         = "${var.name}-1"
  cluster_identifier = aws_rds_cluster.main.id
  engine             = aws_rds_cluster.main.engine
  engine_version     = aws_rds_cluster.main.engine_version
  instance_class     = "db.serverless"
}

resource "aws_secretsmanager_secret" "db_local" {
  count                   = var.use_floci ? 1 : 0
  name                    = "${var.name}/db"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "db_local" {
  count         = var.use_floci ? 1 : 0
  secret_id     = aws_secretsmanager_secret.db_local[0].id
  secret_string = jsonencode({ username = var.db_username, password = var.local_db_password })
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

# Execution role: pull image, write logs, inject the Stripe and DB secrets.
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
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["secretsmanager:GetSecretValue"]
      Resource = [aws_secretsmanager_secret.stripe.arn, local.db_secret_arn]
    }]
  })
}

# Job role: what the application code itself may do. The DB is reached over the network
# with a password, so no AWS API permissions are needed today.
resource "aws_iam_role" "job" {
  name               = "${var.name}-job"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
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
    subnets            = var.use_floci ? ["subnet-floci"] : var.subnet_ids # SDK requires one; Floci ignores it
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
  for_each              = local.jobs
  name                  = "${var.name}-${each.key}"
  type                  = "container"
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
    # Sorted by name: Batch returns them sorted, so any other order shows a permanent diff.
    environment = [
      { name = "DB_HOST", value = aws_rds_cluster.main.endpoint },
      { name = "DB_NAME", value = var.db_name },
      { name = "DB_PORT", value = tostring(aws_rds_cluster.main.port) },
      { name = "DB_USER", value = var.db_username },
      { name = "MAX_ATTEMPTS", value = "4" },
      { name = "STRIPE_API_BASE", value = var.stripe_api_base },
    ]
    secrets = [
      { name = "STRIPE_API_KEY", valueFrom = aws_secretsmanager_secret.stripe.arn },
      { name = "DB_PASSWORD", valueFrom = "${local.db_secret_arn}:password::" },
    ]
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
  for_each            = local.scheduled_jobs
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
output "db_endpoint" { value = aws_rds_cluster.main.endpoint }
