package payment

import (
	"context"
	"net/http"
	"os"
	"testing"
)

// Runs against stripe-mock (STRIPE_API_BASE=http://localhost:12111); skipped otherwise.
func TestChargeAgainstStripeMock(t *testing.T) {
	base := os.Getenv("STRIPE_API_BASE")
	if base == "" {
		t.Skip("STRIPE_API_BASE not set")
	}
	if _, err := http.Get(base); err != nil {
		t.Skipf("stripe-mock not reachable: %v", err)
	}
	s := NewStripe("sk_test_123", base)
	res := s.Charge(context.Background(), ChargeRequest{ChargeID: "cus_x#2026-10", Attempt: 1, CustomerID: "cus_x",
		PaymentMethodID: "pm_card_visa", Amount: 1000, Currency: "jpy", IdempotencyKey: "k1"})
	if res.Err != nil {
		t.Fatalf("charge: %v", res.Err)
	}
	if res.PaymentIntentID == "" || res.Status == "" {
		t.Fatalf("empty result: %+v", res)
	}
	st, err := s.PaymentIntentStatus(context.Background(), res.PaymentIntentID)
	if err != nil || st == "" {
		t.Fatalf("status: %q %v", st, err)
	}
	if _, _, err := s.FindPaymentIntent(context.Background(), "cus_x", "cus_x#2026-10", 1, 0); err != nil {
		t.Fatalf("find: %v", err)
	}
	t.Logf("pi=%s charge-status=%s get-status=%s", res.PaymentIntentID, res.Status, st)
}
