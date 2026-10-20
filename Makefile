export STRIPE_API_BASE ?= http://localhost:12111
# Floci's Aurora MySQL cluster (created by `make db`); the first cluster gets proxy port 7001.
DB_CLUSTER ?= credit-billing-local
export DB_HOST ?= 127.0.0.1
export DB_PORT ?= 7001
export DB_USER ?= admin
export DB_PASSWORD ?= localpass123
export DB_NAME ?= billing

# Terraform target: TF_ENV=local (Floci, default) or TF_ENV=aws (real AWS; vars from terraform/aws.tfvars).
# Each env keeps its own state via a Terraform workspace of the same name.
TF_ENV ?= local
ifeq ($(TF_ENV),local)
TF_VARS := -var use_floci=true -var stripe_api_base=http://stripe-mock:12111
TF_APPROVE := -auto-approve
else
TF_VARS := -var-file=aws.tfvars
endif
TF := cd terraform && terraform

.PHONY: up down db test migrate seed bill retry reconcile tf-init tf-plan tf-apply
up:        ## start Floci + stripe-mock
	docker compose up -d
down:
	docker compose down -v
# Floci credentials only for `make db`, so they never leak into TF_ENV=aws.
db: export AWS_ENDPOINT_URL = http://localhost:4566
db: export AWS_ACCESS_KEY_ID = test
db: export AWS_SECRET_ACCESS_KEY = test
db: export AWS_REGION = ap-northeast-1
db:        ## create an Aurora MySQL cluster + writer instance in Floci (idempotent)
	aws rds describe-db-clusters --db-cluster-identifier $(DB_CLUSTER) >/dev/null 2>&1 || \
	aws rds create-db-cluster --db-cluster-identifier $(DB_CLUSTER) --engine aurora-mysql \
		--master-username $(DB_USER) --master-user-password $(DB_PASSWORD) --database-name $(DB_NAME) >/dev/null
	aws rds describe-db-instances --db-instance-identifier $(DB_CLUSTER)-1 >/dev/null 2>&1 || \
	aws rds create-db-instance --db-instance-identifier $(DB_CLUSTER)-1 --db-cluster-identifier $(DB_CLUSTER) \
		--engine aurora-mysql --db-instance-class db.serverless >/dev/null
	aws rds describe-db-clusters --db-cluster-identifier $(DB_CLUSTER) \
		--query 'DBClusters[0].[Status,Endpoint,Port]' --output text
test:
	go test ./...
migrate:   ## create tables
	go run ./cmd/batch migrate
seed:      ## create tables + sample subscriptions
	go run ./cmd/batch seed
bill retry reconcile:
	go run ./cmd/batch $@
tf-init:
	$(TF) init -input=false && terraform workspace select -or-create $(TF_ENV)
tf-plan: tf-init   ## show Terraform changes (TF_ENV=local|aws)
	$(TF) plan $(TF_VARS)
tf-apply: tf-init  ## apply Terraform (auto-approved only for TF_ENV=local)
	$(TF) apply $(TF_APPROVE) $(TF_VARS)
