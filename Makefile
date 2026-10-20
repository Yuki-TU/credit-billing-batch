export AWS_ENDPOINT_URL ?= http://localhost:4566
export AWS_ACCESS_KEY_ID ?= test
export AWS_SECRET_ACCESS_KEY ?= test
export AWS_REGION ?= ap-northeast-1
export STRIPE_API_BASE ?= http://localhost:12111
export SUBSCRIPTIONS_TABLE ?= credit-billing-subscriptions
export CHARGES_TABLE ?= credit-billing-charges

.PHONY: up down test seed bill retry reconcile tf-local
up:        ## start Floci + stripe-mock
	docker compose up -d
down:
	docker compose down -v
test:
	go test ./...
seed:      ## create tables + sample subscriptions in Floci
	go run ./cmd/batch seed
bill retry reconcile:
	go run ./cmd/batch $@
tf-local:  ## apply Terraform against Floci
	cd terraform && terraform init && terraform apply -auto-approve -var use_floci=true -var stripe_api_base=http://stripe-mock:12111
