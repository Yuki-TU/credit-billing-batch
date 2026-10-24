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

// unknownGrace is how long reconcile waits before looking up an UNKNOWN attempt in Stripe,
// giving Stripe time to settle a request that hit a 5xx. unknownLookback widens the
// PaymentIntent search window before the charge's updated_at (the attempt ran just before it).
const (
	unknownGrace    = time.Hour
	unknownLookback = time.Hour
)

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

// Retry re-attempts FAILED charges whose next_retry_at has passed, and PENDING charges that
// never got a PaymentIntent (bill crashed before saving the result; the same attempt number,
// and so the same idempotency key, is reused). PENDING charges with a PaymentIntent are
// waiting for Stripe and are left to Reconcile, as are UNKNOWN ones.
func (r *Runner) Retry(ctx context.Context) error {
	failed, err := r.Store.ChargesByStatus(ctx, store.StatusFailed)
	if err != nil {
		return err
	}
	pending, err := r.Store.ChargesByStatus(ctx, store.StatusPending)
	if err != nil {
		return err
	}
	var due []store.Charge
	for _, c := range failed {
		if c.NextRetryAt <= r.now().Unix() {
			due = append(due, c)
		}
	}
	for _, c := range pending {
		if c.PaymentIntentID == "" {
			// The crashed attempt was never persisted (attempts still holds the previous
			// count), so attempt() replays it with the same number and key.
			due = append(due, c)
		}
	}
	n := 0
	for i := range due {
		r.attempt(ctx, &due[i])
		n++
	}
	r.Log.Info("retry done", "retried", n)
	return nil
}

// attempt charges once and persists the outcome.
func (r *Runner) attempt(ctx context.Context, c *store.Charge) {
	c.Attempts++
	// Stable per attempt number: a crash+rerun of the same attempt won't double charge.
	res := r.Pay.Charge(ctx, payment.ChargeRequest{
		ChargeID: c.ChargeID, Attempt: c.Attempts, CustomerID: c.CustomerID, PaymentMethodID: c.PaymentMethodID,
		Amount: c.Amount, Currency: c.Currency, IdempotencyKey: fmt.Sprintf("%s#attempt-%d", c.ChargeID, c.Attempts),
	})
	c.LastError = ""
	if res.Err != nil {
		c.LastError = res.Err.Error()
	}
	r.apply(c, res)
	r.save(ctx, c)
	r.Log.Info("charge attempt", "charge", c.ChargeID, "attempt", c.Attempts, "status", c.Status, "pi", c.PaymentIntentID, "code", res.Code)
}

// apply maps a Stripe result onto the charge's status and retry schedule.
func (r *Runner) apply(c *store.Charge, res payment.Result) {
	if res.PaymentIntentID != "" {
		c.PaymentIntentID = res.PaymentIntentID
	}
	switch {
	case res.Uncertain:
		// Stripe may have charged. Don't retry with a new key; Reconcile finds out what happened.
		c.Status, c.NextRetryAt = store.StatusUnknown, 0
	case res.Err == nil && res.Status == "succeeded":
		c.Status, c.NextRetryAt = store.StatusSucceeded, 0
	case res.Status == "requires_action" || res.Code == payment.CodeAuthenticationRequired:
		// Off-session 3DS comes back as a 402 authentication_required with the PaymentIntent in
		// requires_payment_method. Only the customer can complete it; not retried by batch.
		c.Status, c.NextRetryAt = store.StatusRequiresAction, 0
	case res.Err == nil && (res.Status == "processing" || res.Status == "requires_capture"):
		c.Status, c.NextRetryAt = store.StatusPending, 0 // Reconcile settles it
	default:
		r.fail(c)
	}
}

// fail schedules the next attempt, or gives up after MaxAttempts.
func (r *Runner) fail(c *store.Charge) {
	if c.Attempts >= r.MaxAttempts {
		c.Status, c.NextRetryAt = store.StatusAbandoned, 0
		return
	}
	c.Status = store.StatusFailed
	c.NextRetryAt = r.now().Add(time.Duration(c.Attempts) * r.RetryBackoff).Unix()
}

func (r *Runner) save(ctx context.Context, c *store.Charge) {
	if err := r.Store.UpdateCharge(ctx, *c); err != nil {
		r.Log.Error("persist charge", "charge", c.ChargeID, "err", err)
	}
}

// Reconcile compares Stripe's PaymentIntents with our records and fixes drift:
//   - any charge whose PaymentIntent succeeded becomes SUCCEEDED
//   - PENDING charges whose PaymentIntent failed or was canceled are scheduled for retry
//   - UNKNOWN attempts are looked up by metadata and resolved
func (r *Runner) Reconcile(ctx context.Context) error {
	var fixed, checked int
	for _, st := range []store.Status{store.StatusPending, store.StatusFailed, store.StatusRequiresAction, store.StatusUnknown} {
		cs, err := r.Store.ChargesByStatus(ctx, st)
		if err != nil {
			return err
		}
		for i := range cs {
			c := &cs[i]
			if c.Status == store.StatusUnknown {
				if r.resolveUnknown(ctx, c) {
					checked++
					fixed++
				}
				continue
			}
			if c.PaymentIntentID == "" {
				continue
			}
			checked++
			s, err := r.Pay.PaymentIntentStatus(ctx, c.PaymentIntentID)
			if err != nil {
				r.Log.Error("reconcile fetch", "charge", c.ChargeID, "err", err)
				continue
			}
			prev := c.Status
			switch {
			case s == "succeeded" && c.Status != store.StatusSucceeded:
				c.Status, c.NextRetryAt = store.StatusSucceeded, 0
			case c.Status == store.StatusPending && (s == "requires_payment_method" || s == "canceled"):
				c.LastError = "payment_intent " + s
				r.fail(c)
			default:
				continue
			}
			if err := r.Store.UpdateCharge(ctx, *c); err != nil {
				return err
			}
			fixed++
			r.Log.Warn("reconciled", "charge", c.ChargeID, "pi", c.PaymentIntentID, "from", prev, "to", c.Status)
		}
	}
	r.Log.Info("reconcile done", "checked", checked, "fixed", fixed)
	return nil
}

// resolveUnknown finds the PaymentIntent (if any) that an UNKNOWN attempt created and
// applies its outcome. Returns true if the charge was updated.
func (r *Runner) resolveUnknown(ctx context.Context, c *store.Charge) bool {
	if r.now().Sub(time.Unix(c.UpdatedAt, 0)) < unknownGrace {
		return false
	}
	since := c.UpdatedAt - int64(unknownLookback/time.Second)
	res, found, err := r.Pay.FindPaymentIntent(ctx, c.CustomerID, c.ChargeID, c.Attempts, since)
	if err != nil {
		r.Log.Error("reconcile lookup", "charge", c.ChargeID, "err", err)
		return false
	}
	if found {
		r.apply(c, res)
	} else {
		// Stripe never created a PaymentIntent for this attempt, so the next attempt
		// (with a new idempotency key) cannot double charge.
		r.fail(c)
	}
	r.save(ctx, c)
	r.Log.Warn("resolved unknown attempt", "charge", c.ChargeID, "attempt", c.Attempts, "found", found, "pi", c.PaymentIntentID, "to", c.Status)
	return true
}
