// Command batch runs one billing job per invocation (AWS Batch job on Fargate).
//
//	batch bill [--period YYYY-MM]
//	batch retry
//	batch reconcile
//	batch seed            (local dev only: inserts sample subscriptions)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/Yuki-TU/credit-billing-batch/internal/jobs"
	"github.com/Yuki-TU/credit-billing-batch/internal/payment"
	"github.com/Yuki-TU/credit-billing-batch/internal/store"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: batch <bill|retry|reconcile|seed> [flags]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	period := fs.String("period", time.Now().UTC().Format("2006-01"), "billing period YYYY-MM")
	_ = fs.Parse(os.Args[2:])

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// AWS_ENDPOINT_URL (Floci) is honoured natively by the SDK.
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Error("aws config", "err", err)
		os.Exit(1)
	}
	st := store.New(dynamodb.NewFromConfig(awsCfg), env("SUBSCRIPTIONS_TABLE", "subscriptions"), env("CHARGES_TABLE", "charges"))

	if cmd == "seed" {
		if err := st.EnsureTables(ctx); err != nil {
			log.Error("ensure tables", "err", err)
			os.Exit(1)
		}
		for i, pm := range []string{"pm_card_visa", "pm_card_chargeCustomerFail", "pm_card_mastercard"} {
			err = st.PutSubscription(ctx, store.Subscription{
				CustomerID: "cus_seed_" + strconv.Itoa(i), PaymentMethodID: pm, Amount: 1000, Currency: "jpy", Active: true})
			if err != nil {
				log.Error("seed", "err", err)
				os.Exit(1)
			}
		}
		log.Info("seeded 3 subscriptions")
		return
	}

	attempts, _ := strconv.Atoi(env("MAX_ATTEMPTS", "4"))
	backoff, _ := time.ParseDuration(env("RETRY_BACKOFF", "24h"))
	r := &jobs.Runner{
		Store: st, Log: log, MaxAttempts: attempts, RetryBackoff: backoff,
		// STRIPE_API_BASE points at stripe-mock locally; empty in production.
		Pay: payment.NewStripe(env("STRIPE_API_KEY", "sk_test_dummy"), os.Getenv("STRIPE_API_BASE")),
	}
	switch cmd {
	case "bill":
		err = r.Bill(ctx, *period)
	case "retry":
		err = r.Retry(ctx)
	case "reconcile":
		err = r.Reconcile(ctx)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("job failed", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}
