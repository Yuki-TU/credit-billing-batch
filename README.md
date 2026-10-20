# credit-billing-batch

Stripe クレジットカード課金バッチ。**AWS Batch (Fargate)** 上で動き、データは **Aurora MySQL** に保存する。ローカルでは **Floci**(AWSエミュレータ。Aurora MySQL も本物の MySQL 8.0 コンテナで動く)と **stripe-mock** で検証できる。

## ジョブ
| コマンド | 内容 | 既定スケジュール |
|---|---|---|
| `batch bill [--period YYYY-MM]` | 有効な契約ごとに PaymentIntent(off-session, confirm)を作成して課金。再実行しても二重課金しない | 毎月1日 01:00 UTC |
| `batch retry` | `FAILED` / 古い `PENDING` を、バックオフ経過後に再請求。`MAX_ATTEMPTS` 超過で `ABANDONED` | 毎日 02:00 UTC |
| `batch reconcile` | Stripe 側の PaymentIntent 状態と DB を突合し、`succeeded` なのに未反映のものを修正 | 毎日 03:00 UTC |

| `batch migrate` | テーブル作成(冪等)。デプロイ後に手動で SubmitJob する | なし |

3DS 等で `requires_action` になったものはバッチでは再請求せず、顧客対応待ちとして残す。

二重課金対策: `charges.charge_id`(`customer#period`)の主キー制約で (顧客, 期間) を一意化(重複 INSERT のエラー 1062 を「作成済み」として扱う) + Stripe の Idempotency-Key(`customer#period#attempt-N`)。

## 構成
- `cmd/batch`, `internal/{jobs,payment,store}` — Go 1.24 / stripe-go v82 / go-sql-driver/mysql
- `terraform/` — ECR, Aurora MySQL(Serverless v2、パスワードは RDS 管理の Secrets Manager シークレット), Secrets Manager(Stripeキー), Batch(Fargate計算環境・キュー・ジョブ定義x4), EventBridge Scheduler, IAM, CloudWatch Logs
- `docker-compose.yml` — `floci/floci` (4566、RDS プロキシ 7001-7099) と `stripe/stripe-mock` (12111)

## ローカル実行
```sh
make up            # Floci + stripe-mock 起動
make db            # Floci に Aurora MySQL クラスタ + writer を作成(127.0.0.1:7001)
make test          # DB_* が設定されていれば store の結合テストも MySQL に対して走る
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
terraform apply -var 'vpc_id=vpc-...' -var 'subnet_ids=["subnet-..."]' -var 'security_group_ids=["sg-..."]'
aws secretsmanager put-secret-value --secret-id credit-billing/stripe-api-key --secret-string sk_live_...
aws batch submit-job --job-name credit-billing-migrate --job-queue credit-billing-queue --job-definition credit-billing-migrate
```
Fargate は Stripe へ出るため NAT(または公開IP)が必要。`stripe_api_base` は空のままにする。
Aurora は `subnet_ids` に置かれ、`security_group_ids`(Fargate タスクの SG)からの 3306 のみ許可する。DB パスワードは RDS が生成・管理し、ジョブには `DB_PASSWORD` として注入される。
`db_min_acu = 0` にすると未使用時に自動停止できる(Aurora MySQL 3.08+。停止後の初回接続は約15秒かかる)。

## 検証状況
- Floci の Aurora MySQL に対して `migrate` / `seed` / `bill`(再実行で全件 skip)/ `retry` / `reconcile` と store の結合テストを実行済み。
- `terraform validate` と Floci への `plan` は通過。Aurora 部分(`aws_rds_cluster` + writer)は Floci に `apply` して接続・`migrate` まで確認済み。
- 未検証: 実 AWS への `apply`、Floci 上での Batch/ECS ジョブ実行。Floci の Aurora は中身が素の MySQL 8.0 なので、フェイルオーバーや Aurora 固有の挙動はローカルでは確認できない。
