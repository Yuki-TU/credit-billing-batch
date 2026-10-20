# credit-billing-batch

Stripe クレジットカード課金バッチ。**AWS Batch (Fargate)** 上で動き、ローカルでは **Floci**(AWSエミュレータ)と **stripe-mock** で検証できる。

## ジョブ
| コマンド | 内容 | 既定スケジュール |
|---|---|---|
| `batch bill [--period YYYY-MM]` | 有効な契約ごとに PaymentIntent(off-session, confirm)を作成して課金。再実行しても二重課金しない | 毎月1日 01:00 UTC |
| `batch retry` | `FAILED` / 古い `PENDING` を、バックオフ経過後に再請求。`MAX_ATTEMPTS` 超過で `ABANDONED` | 毎日 02:00 UTC |
| `batch reconcile` | Stripe 側の PaymentIntent 状態と DynamoDB を突合し、`succeeded` なのに未反映のものを修正 | 毎日 03:00 UTC |

3DS 等で `requires_action` になったものはバッチでは再請求せず、顧客対応待ちとして残す。

二重課金対策: DynamoDB の条件付き Put で (顧客, 期間) を一意化 + Stripe の Idempotency-Key(`customer#period#attempt-N`)。

## 構成
- `cmd/batch`, `internal/{jobs,payment,store}` — Go 1.24 / stripe-go v82 / aws-sdk-go-v2
- `terraform/` — ECR, DynamoDB x2, Secrets Manager(Stripeキー), Batch(Fargate計算環境・キュー・ジョブ定義x3), EventBridge Scheduler, IAM, CloudWatch Logs
- `docker-compose.yml` — `floci/floci` (4566) と `stripe/stripe-mock` (12111)

## ローカル実行
```sh
make up            # Floci + stripe-mock 起動
make test
make seed          # テーブル作成 + サンプル契約
make bill          # 課金実行
make retry reconcile
```

## Floci 上に Terraform を適用
```sh
make tf-local      # provider エンドポイントを localhost:4566 に向ける
```
ECR に push するイメージは `docker build -t localhost:4566/... .` 等で用意する(Floci の ECR は `registry:2`)。

## stripe-mock の注意
stripe-mock は**ステートレス**で、レスポンスは固定値(PaymentIntent は `requires_payment_method` が返る)。
そのためローカルでは課金が `FAILED` になるのが正常。成功/失敗/3DS の分岐は `internal/jobs` の単体テスト(フェイクGateway)で検証している。状態遷移まで通したい場合は Stripe test mode(`sk_test_...`、`STRIPE_API_BASE` 未設定)を使う。

## 本番(AWS)
```sh
cd terraform
terraform apply -var 'subnet_ids=["subnet-..."]' -var 'security_group_ids=["sg-..."]'
aws secretsmanager put-secret-value --secret-id credit-billing/stripe-api-key --secret-string sk_live_...
```
Fargate は Stripe へ出るため NAT(または公開IP)が必要。`stripe_api_base` は空のままにする。

## 未検証の点
このリポジトリはサンドボックス(Docker デーモンなし・terraform なし)で作成したため、Floci 上での Batch/ECS 実行、`terraform validate/apply` は未実行。Go のテストと stripe-mock への実呼び出しのみ確認済み。
