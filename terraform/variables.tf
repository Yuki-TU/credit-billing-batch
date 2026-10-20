variable "region" {
  type    = string
  default = "ap-northeast-1"
}
variable "name" {
  type    = string
  default = "credit-billing"
}
variable "use_floci" {
  description = "Point the AWS provider at Floci (http://localhost:4566) instead of real AWS."
  type        = bool
  default     = false
}
variable "image" {
  description = "Container image URI for the batch job (ECR). Leave empty to use the repo created here with tag 'latest'."
  type        = string
  default     = ""
}
variable "subnet_ids" {
  description = "Private subnets for Fargate tasks (need egress to api.stripe.com via NAT)."
  type        = list(string)
  default     = []
}
variable "security_group_ids" {
  type    = list(string)
  default = []
}
variable "stripe_api_base" {
  description = "Override Stripe base URL (stripe-mock). Empty in production."
  type        = string
  default     = ""
}
variable "stripe_secret_value" {
  description = "Stripe secret key. Use a throwaway value locally; in prod set the secret out-of-band and ignore."
  type        = string
  default     = "sk_test_dummy"
  sensitive   = true
}
variable "billing_cron" {
  type    = string
  default = "cron(0 1 1 * ? *)" # 01:00 UTC on the 1st of each month
}
variable "retry_cron" {
  type    = string
  default = "cron(0 2 * * ? *)" # daily
}
variable "reconcile_cron" {
  type    = string
  default = "cron(0 3 * * ? *)" # daily
}
