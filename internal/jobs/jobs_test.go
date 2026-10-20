package jobs

import (
	"context"
	"errors"
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
	piState map[string]string
}

func (f *fakePay) Charge(_ context.Context, _, pm string, _ int64, _, _ string) payment.Result {
	f.calls++
	return f.results[pm]
}
func (f *fakePay) PaymentIntentStatus(_ context.Context, id string) (string, error) {
	return f.piState[id], nil
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
