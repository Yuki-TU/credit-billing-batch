// Package payment wraps Stripe PaymentIntents for off-session card charges.
package payment

import (
	"context"
	"errors"
	"fmt"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/client"
)

type Result struct {
	PaymentIntentID string
	Status          string // Stripe PaymentIntent status
	Err             error  // card/API error, if any
}

type Gateway interface {
	// Charge creates and confirms an off-session PaymentIntent. idemKey must be stable per attempt.
	Charge(ctx context.Context, customerID, paymentMethodID string, amount int64, currency, idemKey string) Result
	// PaymentIntentStatus fetches the current status from Stripe (used by reconciliation).
	PaymentIntentStatus(ctx context.Context, id string) (string, error)
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

func (s *Stripe) Charge(ctx context.Context, customerID, pmID string, amount int64, currency, idemKey string) Result {
	p := &stripe.PaymentIntentParams{
		Amount:        stripe.Int64(amount),
		Currency:      stripe.String(currency),
		Customer:      stripe.String(customerID),
		PaymentMethod: stripe.String(pmID),
		Confirm:       stripe.Bool(true),
		OffSession:    stripe.Bool(true),
	}
	p.Context = ctx
	p.SetIdempotencyKey(idemKey)
	pi, err := s.c.PaymentIntents.New(p)
	if err != nil {
		res := Result{Err: err}
		var se *stripe.Error
		if errors.As(err, &se) && se.PaymentIntent != nil { // card declined / auth required
			res.PaymentIntentID, res.Status = se.PaymentIntent.ID, string(se.PaymentIntent.Status)
		}
		return res
	}
	return Result{PaymentIntentID: pi.ID, Status: string(pi.Status)}
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
