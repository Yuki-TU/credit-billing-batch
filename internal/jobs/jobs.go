// Package jobs implements the three batch jobs: monthly billing, retry, reconcile.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Yuki-TU/credit-billing-batch/internal/payment"
	"github.com/Yuki-TU/credit-billing-batch/internal/store"
)

// Store is the persistence the jobs need (implemented by *store.Store).
type Store interface {
	ActiveSubscriptions(ctx context.Context) ([]store.Subscription, error)
	CreateCharge(ctx context.Context, c store.Charge) error
	UpdateCharge(ctx context.Context, c store.Charge) error
	ChargesByStatus(ctx context.Context, st store.Status) ([]store.Charge, error)
}

type Runner struct {
	Store        Store
	Pay          payment.Gateway
	Log          *slog.Logger
	MaxAttempts  int
	RetryBackoff time.Duration // delay between retries (multiplied by attempt count)
	Now          func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Bill creates and attempts one charge per active subscription for the period (YYYY-MM).
// Re-running is safe: existing (customer, period) charges are skipped.
func (r *Runner) Bill(ctx context.Context, period string) error {
	subs, err := r.Store.ActiveSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	var created, skipped, failed int
	for _, s := range subs {
		c := store.Charge{
			ChargeID: store.ChargeID(s.CustomerID, period), CustomerID: s.CustomerID, Period: period,
			PaymentMethodID: s.PaymentMethodID, Amount: s.Amount, Currency: s.Currency,
		}
		if err := r.Store.CreateCharge(ctx, c); err != nil {
			if errors.Is(err, store.ErrExists) {
				skipped++
				continue
			}
			return fmt.Errorf("create charge %s: %w", c.ChargeID, err)
		}
		c.Status = store.StatusPending
		r.attempt(ctx, &c)
		created++
		if c.Status != store.StatusSucceeded {
			failed++
		}
	}
	r.Log.Info("bill done", "period", period, "created", created, "skipped", skipped, "not_succeeded", failed)
	return nil
}

// Retry re-attempts FAILED and stale PENDING charges whose next_retry_at has passed.
func (r *Runner) Retry(ctx context.Context) error {
	var due []store.Charge
	for _, st := range []store.Status{store.StatusFailed, store.StatusPending} {
		cs, err := r.Store.ChargesByStatus(ctx, st)
		if err != nil {
			return err
		}
		due = append(due, cs...)
	}
	n := 0
	for i := range due {
		c := due[i]
		if c.NextRetryAt > r.now().Unix() {
			continue
		}
		r.attempt(ctx, &c)
		n++
	}
	r.Log.Info("retry done", "retried", n)
	return nil
}

// attempt charges once and persists the outcome.
func (r *Runner) attempt(ctx context.Context, c *store.Charge) {
	c.Attempts++
	// Stable per attempt number: a crash+rerun of the same attempt won't double charge.
	key := fmt.Sprintf("%s#attempt-%d", c.ChargeID, c.Attempts)
	res := r.Pay.Charge(ctx, c.CustomerID, c.PaymentMethodID, c.Amount, c.Currency, key)
	if res.PaymentIntentID != "" {
		c.PaymentIntentID = res.PaymentIntentID
	}
	c.LastError = ""
	switch {
	case res.Err == nil && res.Status == "succeeded":
		c.Status = store.StatusSucceeded
		c.NextRetryAt = 0
	case res.Status == "requires_action":
		c.Status = store.StatusRequiresAction // needs customer; not retried by batch
	case res.Err == nil && (res.Status == "processing" || res.Status == "requires_capture"):
		c.Status = store.StatusPending // reconcile will settle it
		c.NextRetryAt = r.now().Add(r.RetryBackoff).Unix()
	default:
		if res.Err != nil {
			c.LastError = res.Err.Error()
		}
		if c.Attempts >= r.MaxAttempts {
			c.Status = store.StatusAbandoned
			c.NextRetryAt = 0
		} else {
			c.Status = store.StatusFailed
			c.NextRetryAt = r.now().Add(time.Duration(c.Attempts) * r.RetryBackoff).Unix()
		}
	}
	if err := r.Store.UpdateCharge(ctx, *c); err != nil {
		r.Log.Error("persist charge", "charge", c.ChargeID, "err", err)
	}
	r.Log.Info("charge attempt", "charge", c.ChargeID, "attempt", c.Attempts, "status", c.Status, "pi", c.PaymentIntentID)
}

// Reconcile compares Stripe's PaymentIntent status with our records and fixes drift.
func (r *Runner) Reconcile(ctx context.Context) error {
	var fixed, checked int
	for _, st := range []store.Status{store.StatusPending, store.StatusFailed, store.StatusRequiresAction} {
		cs, err := r.Store.ChargesByStatus(ctx, st)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.PaymentIntentID == "" {
				continue
			}
			checked++
			s, err := r.Pay.PaymentIntentStatus(ctx, c.PaymentIntentID)
			if err != nil {
				r.Log.Error("reconcile fetch", "charge", c.ChargeID, "err", err)
				continue
			}
			if s == "succeeded" && c.Status != store.StatusSucceeded {
				c.Status, c.NextRetryAt = store.StatusSucceeded, 0
				if err := r.Store.UpdateCharge(ctx, c); err != nil {
					return err
				}
				fixed++
				r.Log.Warn("reconciled to succeeded", "charge", c.ChargeID, "pi", c.PaymentIntentID)
			}
		}
	}
	r.Log.Info("reconcile done", "checked", checked, "fixed", fixed)
	return nil
}
