package payment

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// fakeStripe is a minimal Stripe API for checking what we send and how responses are classified.
type fakeStripe struct {
	mu       sync.Mutex
	status   int    // response status for POST /v1/payment_intents
	body     string // response body for POST /v1/payment_intents
	posts    int
	lastForm url.Values
	lastKey  string
	listQ    url.Values
	listBody string
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents":
		f.posts++
		b, _ := io.ReadAll(r.Body)
		f.lastForm, _ = url.ParseQuery(string(b))
		f.lastKey = r.Header.Get("Idempotency-Key")
		w.Header().Set("Stripe-Should-Retry", "false") // keep the SDK from retrying in tests
		w.WriteHeader(f.status)
		io.WriteString(w, f.body)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/payment_intents":
		f.listQ = r.URL.Query()
		io.WriteString(w, f.listBody)
	default:
		http.NotFound(w, r)
	}
}

func newFake(t *testing.T, f *fakeStripe) *Stripe {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewStripe("sk_test_123", srv.URL)
}

var req = ChargeRequest{ChargeID: "cus_1#2026-10", Attempt: 2, CustomerID: "cus_1", PaymentMethodID: "pm_1",
	Amount: 1000, Currency: "jpy", IdempotencyKey: "cus_1#2026-10#attempt-2"}

func TestChargeSendsOffSessionRequest(t *testing.T) {
	f := &fakeStripe{status: 200, body: `{"id":"pi_1","object":"payment_intent","status":"succeeded"}`}
	res := newFake(t, f).Charge(context.Background(), req)
	if res.Err != nil || res.PaymentIntentID != "pi_1" || res.Status != "succeeded" || res.Uncertain {
		t.Fatalf("result: %+v", res)
	}
	want := map[string]string{
		"amount": "1000", "currency": "jpy", "customer": "cus_1", "payment_method": "pm_1",
		"confirm": "true", "off_session": "true",
		"metadata[charge_id]": "cus_1#2026-10", "metadata[attempt]": "2",
	}
	for k, v := range want {
		if got := f.lastForm.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if f.lastKey != req.IdempotencyKey {
		t.Errorf("Idempotency-Key = %q", f.lastKey)
	}
}

func TestChargeClassifiesErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantCode  string
		wantPI    string
		uncertain bool
	}{
		{"3DS required off-session", 402, `{"error":{"type":"card_error","code":"authentication_required","decline_code":"authentication_required",
			"message":"auth","payment_intent":{"id":"pi_3ds","object":"payment_intent","status":"requires_payment_method"}}}`,
			CodeAuthenticationRequired, "pi_3ds", false},
		{"card declined", 402, `{"error":{"type":"card_error","code":"card_declined","decline_code":"insufficient_funds",
			"message":"no","payment_intent":{"id":"pi_d","object":"payment_intent","status":"requires_payment_method"}}}`,
			"insufficient_funds", "pi_d", false},
		{"server error", 500, `{"error":{"type":"api_error","message":"boom"}}`, "", "", true},
		{"idempotency conflict", 409, `{"error":{"type":"idempotency_error","message":"in progress"}}`, "", "", true},
		{"invalid request", 400, `{"error":{"type":"invalid_request_error","code":"parameter_missing","message":"x"}}`, "parameter_missing", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStripe{status: tc.status, body: tc.body}
			res := newFake(t, f).Charge(context.Background(), req)
			if res.Err == nil || res.Code != tc.wantCode || res.PaymentIntentID != tc.wantPI || res.Uncertain != tc.uncertain {
				t.Fatalf("result: %+v", res)
			}
		})
	}
}

func TestChargeWithoutResponseIsUncertain(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listening: no response at all
	s := NewStripe("sk_test_123", base)
	// Cancel up front so the SDK doesn't sleep between its own retries; either way the
	// error carries no Stripe response, which is what makes the outcome unknown.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := s.Charge(ctx, req); res.Err == nil || !res.Uncertain {
		t.Fatalf("result: %+v", res)
	}
}

func TestFindPaymentIntentMatchesMetadata(t *testing.T) {
	f := &fakeStripe{listBody: `{"object":"list","has_more":false,"data":[
		{"id":"pi_other","object":"payment_intent","status":"succeeded","metadata":{"charge_id":"cus_1#2026-09","attempt":"2"}},
		{"id":"pi_prev","object":"payment_intent","status":"requires_payment_method","metadata":{"charge_id":"cus_1#2026-10","attempt":"1"}},
		{"id":"pi_hit","object":"payment_intent","status":"requires_payment_method","metadata":{"charge_id":"cus_1#2026-10","attempt":"2"},
		 "last_payment_error":{"type":"card_error","code":"authentication_required","decline_code":"authentication_required"}}]}`}
	s := newFake(t, f)
	res, found, err := s.FindPaymentIntent(context.Background(), "cus_1", "cus_1#2026-10", 2, 1700000000)
	if err != nil || !found || res.PaymentIntentID != "pi_hit" || res.Status != "requires_payment_method" || res.Code != CodeAuthenticationRequired {
		t.Fatalf("found=%v err=%v res=%+v", found, err, res)
	}
	if f.listQ.Get("customer") != "cus_1" || f.listQ.Get("created[gte]") != "1700000000" {
		t.Errorf("list query: %v", f.listQ)
	}
	if _, found, _ := s.FindPaymentIntent(context.Background(), "cus_1", "cus_1#2026-10", 3, 0); found {
		t.Error("found an attempt that was never created")
	}
}
