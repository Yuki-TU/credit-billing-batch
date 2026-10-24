// Package payment wraps Stripe PaymentIntents for off-session card charges.
package payment

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/client"
)

// Metadata keys set on every PaymentIntent so an attempt can be found again
// when its create call ended with an unknown outcome.
const (
	MetaChargeID = "charge_id"
	MetaAttempt  = "attempt"
)

// CodeAuthenticationRequired is returned for off-session payments the issuer wants the
// customer to authenticate (3DS). Stripe reports it as a 402 with the PaymentIntent back
// in requires_payment_method, not requires_action.
const CodeAuthenticationRequired = "authentication_required"

type ChargeRequest struct {
	ChargeID        string // our charge_id, stored in PaymentIntent metadata
	Attempt         int
	CustomerID      string
	PaymentMethodID string
	Amount          int64
	Currency        string
	IdempotencyKey  string // must be stable per attempt
}

type Result struct {
	PaymentIntentID string
	Status          string // Stripe PaymentIntent status
	Code            string // decline code, else error code (e.g. authentication_required)
	Err             error  // card/API error, if any
	// Uncertain means Stripe may or may not have executed the request (5xx, 409 or no
	// response). The attempt must not be retried with a new idempotency key until the
	// outcome is known; see FindPaymentIntent.
	Uncertain bool
}

type Gateway interface {
	// Charge creates and confirms an off-session PaymentIntent.
	Charge(ctx context.Context, req ChargeRequest) Result
	// PaymentIntentStatus fetches the current status from Stripe (used by reconciliation).
	PaymentIntentStatus(ctx context.Context, id string) (string, error)
	// FindPaymentIntent looks up the PaymentIntent created for (chargeID, attempt) among the
	// customer's PaymentIntents created at or after since (unix seconds). The Result carries
	// its ID, status and last payment error code; Err is never set.
	FindPaymentIntent(ctx context.Context, customerID, chargeID string, attempt int, since int64) (res Result, found bool, err error)
}

type Stripe struct{ c *client.API }

// NewStripe builds a client. If baseURL is set (e.g. http://stripe-mock:12111), all calls go there.
func NewStripe(apiKey, baseURL string) *Stripe {
	cfg := &stripe.BackendConfig{MaxNetworkRetries: stripe.Int64(2)}
	if baseURL != "" {
		cfg.URL = stripe.String(baseURL)
	}
	b := stripe.GetBackendWithConfig(stripe.APIBackend, cfg)
	return &Stripe{c: client.New(apiKey, &stripe.Backends{API: b, Uploads: b, Connect: b})}
}

func (s *Stripe) Charge(ctx context.Context, req ChargeRequest) Result {
	p := &stripe.PaymentIntentParams{
		Amount:        stripe.Int64(req.Amount),
		Currency:      stripe.String(req.Currency),
		Customer:      stripe.String(req.CustomerID),
		PaymentMethod: stripe.String(req.PaymentMethodID),
		Confirm:       stripe.Bool(true),
		OffSession:    stripe.Bool(true),
	}
	p.Context = ctx
	p.SetIdempotencyKey(req.IdempotencyKey)
	p.AddMetadata(MetaChargeID, req.ChargeID)
	p.AddMetadata(MetaAttempt, strconv.Itoa(req.Attempt))
	pi, err := s.c.PaymentIntents.New(p)
	if err != nil {
		return resultFromError(err)
	}
	return Result{PaymentIntentID: pi.ID, Status: string(pi.Status)}
}

// resultFromError classifies a failed create call.
func resultFromError(err error) Result {
	res := Result{Err: err}
	var se *stripe.Error
	if !errors.As(err, &se) {
		// Network failure after the SDK's own retries, or ctx cancelled: no response.
		res.Uncertain = true
		return res
	}
	if se.PaymentIntent != nil { // card declined / auth required
		res.PaymentIntentID, res.Status = se.PaymentIntent.ID, string(se.PaymentIntent.Status)
	}
	res.Code = errorCode(se)
	// 5xx: result is indeterminate. 409: the same key is still executing elsewhere.
	res.Uncertain = se.HTTPStatusCode >= 500 || se.HTTPStatusCode == 409
	return res
}

// errorCode prefers the issuer's decline code over the generic error code.
func errorCode(se *stripe.Error) string {
	if se == nil {
		return ""
	}
	if se.DeclineCode != "" {
		return string(se.DeclineCode)
	}
	return string(se.Code)
}

func (s *Stripe) PaymentIntentStatus(ctx context.Context, id string) (string, error) {
	p := &stripe.PaymentIntentParams{}
	p.Context = ctx
	pi, err := s.c.PaymentIntents.Get(id, p)
	if err != nil {
		return "", fmt.Errorf("get payment intent %s: %w", id, err)
	}
	return string(pi.Status), nil
}

func (s *Stripe) FindPaymentIntent(ctx context.Context, customerID, chargeID string, attempt int, since int64) (Result, bool, error) {
	p := &stripe.PaymentIntentListParams{
		Customer:     stripe.String(customerID),
		CreatedRange: &stripe.RangeQueryParams{GreaterThanOrEqual: since},
	}
	p.Context = ctx
	want := strconv.Itoa(attempt)
	it := s.c.PaymentIntents.List(p)
	for it.Next() {
		pi := it.PaymentIntent()
		if pi.Metadata[MetaChargeID] == chargeID && pi.Metadata[MetaAttempt] == want {
			return Result{PaymentIntentID: pi.ID, Status: string(pi.Status), Code: errorCode(pi.LastPaymentError)}, true, nil
		}
	}
	if err := it.Err(); err != nil {
		return Result{}, false, fmt.Errorf("list payment intents for %s: %w", customerID, err)
	}
	return Result{}, false, nil
}
