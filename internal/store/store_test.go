package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Runs against MySQL (Floci's Aurora MySQL locally: DB_HOST etc. from the Makefile); skipped otherwise.
func TestStoreAgainstMySQL(t *testing.T) {
	if os.Getenv("DB_HOST") == "" {
		t.Skip("DB_HOST not set")
	}
	ctx := context.Background()
	db, err := Open(ctx, os.Getenv("DB_HOST"), os.Getenv("DB_PORT"), os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD"), os.Getenv("DB_NAME"))
	if err != nil {
		t.Skipf("mysql not reachable: %v", err)
	}
	defer db.Close()
	s := New(db)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cus := "cus_test_" + time.Now().Format("150405.000000")
	id := ChargeID(cus, "2026-10")
	t.Cleanup(func() { db.Exec(`DELETE FROM charges WHERE charge_id = ?`, id) })

	c := Charge{ChargeID: id, CustomerID: cus, Period: "2026-10", PaymentMethodID: "pm_x", Amount: 1000, Currency: "jpy"}
	if err := s.CreateCharge(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateCharge(ctx, c); !errors.Is(err, ErrExists) {
		t.Fatalf("second create: want ErrExists, got %v", err)
	}

	c.Status, c.Attempts, c.PaymentIntentID, c.LastError, c.NextRetryAt = StatusFailed, 1, "pi_1", "declined", 42
	if err := s.UpdateCharge(ctx, c); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetCharge(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusFailed || got.Attempts != 1 || got.PaymentIntentID != "pi_1" || got.LastError != "declined" || got.NextRetryAt != 42 {
		t.Fatalf("unexpected charge: %+v", got)
	}

	failed, err := s.ChargesByStatus(ctx, StatusFailed)
	if err != nil {
		t.Fatalf("by status: %v", err)
	}
	found := false
	for _, f := range failed {
		found = found || f.ChargeID == id
	}
	if !found {
		t.Fatalf("charge %s not listed as FAILED", id)
	}
}
