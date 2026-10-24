package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Yuki-TU/credit-billing-batch/internal/payment"
	"github.com/Yuki-TU/credit-billing-batch/internal/store"
)

type memStore struct {
	subs    []store.Subscription
	charges map[string]store.Charge
}

func (m *memStore) ActiveSubscriptions(context.Context) ([]store.Subscription, error) {
	return m.subs, nil
}
func (m *memStore) CreateCharge(_ context.Context, c store.Charge) error {
	if _, ok := m.charges[c.ChargeID]; ok {
		return store.ErrExists
	}
	c.Status = store.StatusPending
	m.charges[c.ChargeID] = c
	return nil
}
func (m *memStore) UpdateCharge(_ context.Context, c store.Charge) error {
	m.charges[c.ChargeID] = c
	return nil
}
func (m *memStore) ChargesByStatus(_ context.Context, st store.Status) ([]store.Charge, error) {
	var out []store.Charge
	for _, c := range m.charges {
		if c.Status == st {
			out = append(out, c)
		}
	}
	return out, nil
}

type fakePay struct {
	results map[string]payment.Result // by payment method
	calls   int
	keys    []string // idempotency keys, in call order
	piState map[string]string
	found   map[string]payment.Result // FindPaymentIntent results by charge_id#attempt
	lookups int
}

func (f *fakePay) Charge(_ context.Context, req payment.ChargeRequest) payment.Result {
	f.calls++
	f.keys = append(f.keys, req.IdempotencyKey)
	return f.results[req.PaymentMethodID]
}
func (f *fakePay) PaymentIntentStatus(_ context.Context, id string) (string, error) {
	return f.piState[id], nil
}
func (f *fakePay) FindPaymentIntent(_ context.Context, _, chargeID string, attempt int, _ int64) (payment.Result, bool, error) {
	f.lookups++
	res, ok := f.found[fmt.Sprintf("%s#%d", chargeID, attempt)]
	return res, ok, nil
}

func newRunner(ms *memStore, fp *fakePay, now time.Time) *Runner {
	return &Runner{Store: ms, Pay: fp, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxAttempts: 2, RetryBackoff: time.Hour, Now: func() time.Time { return now }}
}

func TestBillIsIdempotentAndHandlesOutcomes(t *testing.T) {
	ms := &memStore{charges: map[string]store.Charge{}, subs: []store.Subscription{
		{CustomerID: "ok", PaymentMethodID: "pm_ok", Amount: 100, Currency: "jpy", Active: true},
		{CustomerID: "bad", PaymentMethodID: "pm_bad", Amount: 100, Currency: "jpy", Active: true},
		{CustomerID: "3ds", PaymentMethodID: "pm_3ds", Amount: 100, Currency: "jpy", Active: true},
	}}
	fp := &fakePay{results: map[string]payment.Result{
		"pm_ok":  {PaymentIntentID: "pi_ok", Status: "succeeded"},
		"pm_bad": {PaymentIntentID: "pi_bad", Status: "requires_payment_method", Err: errors.New("declined")},
		"pm_3ds": {PaymentIntentID: "pi_3ds", Status: "requires_action", Err: errors.New("auth required")},
	}}
	r := newRunner(ms, fp, time.Unix(1000, 0))
	if err := r.Bill(context.Background(), "2026-10"); err != nil {
		t.Fatal(err)
	}
	want := map[string]store.Status{"ok#2026-10": store.StatusSucceeded, "bad#2026-10": store.StatusFailed, "3ds#2026-10": store.StatusRequiresAction}
	for id, st := range want {
		if got := ms.charges[id].Status; got != st {
			t.Errorf("%s: got %s want %s", id, got, st)
		}
	}
	// re-run must not charge again
	calls := fp.calls
	_ = r.Bill(context.Background(), "2026-10")
	if fp.calls != calls {
		t.Errorf("rerun charged again: %d -> %d", calls, fp.calls)
	}
}

func TestRetryBackoffAndAbandon(t *testing.T) {
	now := time.Unix(1000, 0)
	ms := &memStore{charges: map[string]store.Charge{}, subs: []store.Subscription{
		{CustomerID: "bad", PaymentMethodID: "pm_bad", Amount: 100, Currency: "jpy", Active: true}}}
	fp := &fakePay{results: map[string]payment.Result{"pm_bad": {Status: "requires_payment_method", Err: errors.New("declined")}}}
	r := newRunner(ms, fp, now)
	_ = r.Bill(context.Background(), "2026-10")
	c := ms.charges["bad#2026-10"]
	if c.Status != store.StatusFailed || c.Attempts != 1 || c.NextRetryAt != now.Add(time.Hour).Unix() {
		t.Fatalf("after bill: %+v", c)
	}
	_ = r.Retry(context.Background()) // not due yet
	if ms.charges["bad#2026-10"].Attempts != 1 {
		t.Fatal("retried before due")
	}
	r.Now = func() time.Time { return now.Add(2 * time.Hour) }
	_ = r.Retry(context.Background())
	if c := ms.charges["bad#2026-10"]; c.Status != store.StatusAbandoned || c.Attempts != 2 {
		t.Fatalf("after retry: %+v", c)
	}
}

func TestReconcileFixesDrift(t *testing.T) {
	ms := &memStore{charges: map[string]store.Charge{
		"c#1": {ChargeID: "c#1", Status: store.StatusPending, PaymentIntentID: "pi_1"},
		"c#2": {ChargeID: "c#2", Status: store.StatusFailed, PaymentIntentID: "pi_2"},
	}}
	fp := &fakePay{piState: map[string]string{"pi_1": "succeeded", "pi_2": "requires_payment_method"}}
	_ = newRunner(ms, fp, time.Now()).Reconcile(context.Background())
	if ms.charges["c#1"].Status != store.StatusSucceeded || ms.charges["c#2"].Status != store.StatusFailed {
		t.Fatalf("%+v", ms.charges)
	}
}

// Off-session 3DS: Stripe answers 402 authentication_required with the PaymentIntent back in
// requires_payment_method. It must wait for the customer, not be retried by the batch.
func TestAuthenticationRequiredWaitsForCustomer(t *testing.T) {
	now := time.Unix(1000, 0)
	ms := &memStore{charges: map[string]store.Charge{}, subs: []store.Subscription{
		{CustomerID: "c", PaymentMethodID: "pm_3ds", Amount: 100, Currency: "jpy", Active: true}}}
	fp := &fakePay{results: map[string]payment.Result{"pm_3ds": {PaymentIntentID: "pi_3ds", Status: "requires_payment_method",
		Code: payment.CodeAuthenticationRequired, Err: errors.New("authentication required")}}}
	r := newRunner(ms, fp, now)
	_ = r.Bill(context.Background(), "2026-10")
	c := ms.charges["c#2026-10"]
	if c.Status != store.StatusRequiresAction || c.PaymentIntentID != "pi_3ds" || c.NextRetryAt != 0 {
		t.Fatalf("after bill: %+v", c)
	}
	r.Now = func() time.Time { return now.Add(48 * time.Hour) }
	_ = r.Retry(context.Background())
	if fp.calls != 1 {
		t.Fatalf("retry re-charged a charge that needs the customer: %d calls", fp.calls)
	}
	// customer authenticates later -> reconcile picks it up
	fp.piState = map[string]string{"pi_3ds": "succeeded"}
	_ = r.Reconcile(context.Background())
	if got := ms.charges["c#2026-10"].Status; got != store.StatusSucceeded {
		t.Fatalf("after reconcile: %s", got)
	}
}

// A 5xx / no-response attempt is UNKNOWN: never retried with a new key until reconcile
// has found out whether Stripe created a PaymentIntent.
func TestUncertainAttemptIsResolvedByReconcile(t *testing.T) {
	cases := []struct {
		name      string
		found     *payment.Result
		want      store.Status
		wantPI    string
		wantRetry bool // a new attempt is scheduled
	}{
		{"charged", &payment.Result{PaymentIntentID: "pi_x", Status: "succeeded"}, store.StatusSucceeded, "pi_x", false},
		{"declined", &payment.Result{PaymentIntentID: "pi_x", Status: "requires_payment_method", Code: "card_declined"}, store.StatusFailed, "pi_x", true},
		{"needs 3DS", &payment.Result{PaymentIntentID: "pi_x", Status: "requires_payment_method", Code: payment.CodeAuthenticationRequired}, store.StatusRequiresAction, "pi_x", false},
		{"never created", nil, store.StatusFailed, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(100_000, 0)
			ms := &memStore{charges: map[string]store.Charge{}, subs: []store.Subscription{
				{CustomerID: "c", PaymentMethodID: "pm", Amount: 100, Currency: "jpy", Active: true}}}
			fp := &fakePay{results: map[string]payment.Result{"pm": {Uncertain: true, Err: errors.New("500")}}, found: map[string]payment.Result{}}
			if tc.found != nil {
				fp.found["c#2026-10#1"] = *tc.found
			}
			r := newRunner(ms, fp, now)
			_ = r.Bill(context.Background(), "2026-10")
			c := ms.charges["c#2026-10"]
			if c.Status != store.StatusUnknown || c.Attempts != 1 {
				t.Fatalf("after bill: %+v", c)
			}
			c.UpdatedAt = now.Unix() // memStore doesn't stamp it
			ms.charges[c.ChargeID] = c

			r.Now = func() time.Time { return now.Add(48 * time.Hour) }
			_ = r.Retry(context.Background())
			if fp.calls != 1 {
				t.Fatalf("retry charged an UNKNOWN attempt: %d calls", fp.calls)
			}

			r.Now = func() time.Time { return now.Add(30 * time.Minute) }
			_ = r.Reconcile(context.Background())
			if fp.lookups != 0 || ms.charges[c.ChargeID].Status != store.StatusUnknown {
				t.Fatalf("resolved before grace period: lookups=%d %+v", fp.lookups, ms.charges[c.ChargeID])
			}

			r.Now = func() time.Time { return now.Add(2 * time.Hour) }
			_ = r.Reconcile(context.Background())
			c = ms.charges[c.ChargeID]
			if c.Status != tc.want || c.PaymentIntentID != tc.wantPI || (c.NextRetryAt != 0) != tc.wantRetry {
				t.Fatalf("after reconcile: %+v", c)
			}
			if tc.wantRetry {
				r.Now = func() time.Time { return now.Add(48 * time.Hour) }
				fp.results["pm"] = payment.Result{PaymentIntentID: "pi_2", Status: "succeeded"}
				_ = r.Retry(context.Background())
				if got := fp.keys[len(fp.keys)-1]; got != "c#2026-10#attempt-2" {
					t.Fatalf("next attempt key = %q", got)
				}
			}
		})
	}
}

// PENDING with a PaymentIntent (processing) is Stripe's to settle: retry must not open a
// second PaymentIntent; reconcile moves it on once Stripe decides.
func TestProcessingIsLeftToReconcile(t *testing.T) {
	now := time.Unix(1000, 0)
	ms := &memStore{charges: map[string]store.Charge{}, subs: []store.Subscription{
		{CustomerID: "c", PaymentMethodID: "pm", Amount: 100, Currency: "jpy", Active: true}}}
	fp := &fakePay{results: map[string]payment.Result{"pm": {PaymentIntentID: "pi_p", Status: "processing"}}}
	r := newRunner(ms, fp, now)
	_ = r.Bill(context.Background(), "2026-10")
	r.Now = func() time.Time { return now.Add(72 * time.Hour) }
	_ = r.Retry(context.Background())
	if fp.calls != 1 {
		t.Fatalf("retry re-charged a processing PaymentIntent: %d calls", fp.calls)
	}
	fp.piState = map[string]string{"pi_p": "requires_payment_method"}
	_ = r.Reconcile(context.Background())
	if c := ms.charges["c#2026-10"]; c.Status != store.StatusFailed || c.NextRetryAt == 0 {
		t.Fatalf("after reconcile: %+v", c)
	}
}

// bill crashed after calling Stripe but before saving: the charge is PENDING with no
// PaymentIntent and attempts=0. Retry replays attempt 1 with the same idempotency key.
func TestRetryReplaysCrashedAttemptWithSameKey(t *testing.T) {
	ms := &memStore{charges: map[string]store.Charge{
		"c#2026-10": {ChargeID: "c#2026-10", CustomerID: "c", PaymentMethodID: "pm", Status: store.StatusPending},
	}}
	fp := &fakePay{results: map[string]payment.Result{"pm": {PaymentIntentID: "pi_1", Status: "succeeded"}}}
	_ = newRunner(ms, fp, time.Unix(1000, 0)).Retry(context.Background())
	if len(fp.keys) != 1 || fp.keys[0] != "c#2026-10#attempt-1" {
		t.Fatalf("keys = %v", fp.keys)
	}
	if c := ms.charges["c#2026-10"]; c.Status != store.StatusSucceeded || c.Attempts != 1 {
		t.Fatalf("%+v", c)
	}
}
