// Package store persists subscriptions and charge records in Aurora MySQL.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

type Status string

const (
	StatusPending        Status = "PENDING"
	StatusSucceeded      Status = "SUCCEEDED"
	StatusFailed         Status = "FAILED"          // retryable
	StatusRequiresAction Status = "REQUIRES_ACTION" // 3DS etc. needs the customer
	StatusAbandoned      Status = "ABANDONED"       // retries exhausted
)

type Subscription struct {
	CustomerID      string
	PaymentMethodID string
	Amount          int64
	Currency        string
	Active          bool
}

// Charge is one billing attempt series for (customer, period). ChargeID = customer#period.
type Charge struct {
	ChargeID        string
	CustomerID      string
	Period          string // e.g. 2026-10
	PaymentMethodID string
	Amount          int64
	Currency        string
	Status          Status
	Attempts        int
	PaymentIntentID string // empty until the first attempt reaches Stripe
	LastError       string
	NextRetryAt     int64 // unix seconds; 0 = not scheduled
	UpdatedAt       int64
}

var ErrExists = errors.New("charge already exists")

// MySQL error number for a PRIMARY KEY / UNIQUE violation.
const errDupEntry = 1062

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

// Open connects to MySQL and verifies the connection.
func Open(ctx context.Context, host, port, user, password, dbName string) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.Net, cfg.Addr = "tcp", host+":"+port
	cfg.User, cfg.Passwd, cfg.DBName = user, password, dbName
	cfg.TLSConfig = "preferred" // Aurora offers TLS; local MySQL may use a self-signed cert
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4) // jobs are sequential; keep Aurora connections low
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect %s: %w", cfg.Addr, err)
	}
	return db, nil
}

func ChargeID(customerID, period string) string { return customerID + "#" + period }

// schema is idempotent; applied by `batch migrate` (and `seed` locally).
var schema = []string{
	`CREATE TABLE IF NOT EXISTS subscriptions (
		customer_id       VARCHAR(255) NOT NULL PRIMARY KEY,
		payment_method_id VARCHAR(255) NOT NULL,
		amount            BIGINT       NOT NULL,
		currency          CHAR(3)      NOT NULL,
		active            BOOLEAN      NOT NULL,
		KEY idx_active (active)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS charges (
		charge_id         VARCHAR(255) NOT NULL PRIMARY KEY,
		customer_id       VARCHAR(255) NOT NULL,
		period            CHAR(7)      NOT NULL,
		payment_method_id VARCHAR(255) NOT NULL,
		amount            BIGINT       NOT NULL,
		currency          CHAR(3)      NOT NULL,
		status            VARCHAR(32)  NOT NULL,
		attempts          INT          NOT NULL DEFAULT 0,
		payment_intent_id VARCHAR(255) NOT NULL DEFAULT '',
		last_error        TEXT         NOT NULL,
		next_retry_at     BIGINT       NOT NULL DEFAULT 0,
		updated_at        BIGINT       NOT NULL,
		KEY idx_status (status, next_retry_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// Migrate creates both tables if missing.
func (s *Store) Migrate(ctx context.Context) error {
	for _, q := range schema {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ActiveSubscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT customer_id, payment_method_id, amount, currency, active FROM subscriptions WHERE active`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.CustomerID, &sub.PaymentMethodID, &sub.Amount, &sub.Currency, &sub.Active); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// PutSubscription inserts or overwrites a subscription.
func (s *Store) PutSubscription(ctx context.Context, sub Subscription) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO subscriptions (customer_id, payment_method_id, amount, currency, active) VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE payment_method_id = VALUES(payment_method_id), amount = VALUES(amount),
			currency = VALUES(currency), active = VALUES(active)`,
		sub.CustomerID, sub.PaymentMethodID, sub.Amount, sub.Currency, sub.Active)
	return err
}

// CreateCharge inserts a PENDING charge; returns ErrExists if already created for the period
// (this makes the monthly job safe to re-run).
func (s *Store) CreateCharge(ctx context.Context, c Charge) error {
	c.Status, c.UpdatedAt = StatusPending, time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO charges (charge_id, customer_id, period, payment_method_id, amount, currency, status,
			attempts, payment_intent_id, last_error, next_retry_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ChargeID, c.CustomerID, c.Period, c.PaymentMethodID, c.Amount, c.Currency, c.Status,
		c.Attempts, c.PaymentIntentID, c.LastError, c.NextRetryAt, c.UpdatedAt)
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == errDupEntry {
		return ErrExists
	}
	return err
}

// UpdateCharge persists the mutable fields of an existing charge.
func (s *Store) UpdateCharge(ctx context.Context, c Charge) error {
	c.UpdatedAt = time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE charges SET status = ?, attempts = ?, payment_intent_id = ?, last_error = ?,
			next_retry_at = ?, updated_at = ?
		WHERE charge_id = ?`,
		c.Status, c.Attempts, c.PaymentIntentID, c.LastError, c.NextRetryAt, c.UpdatedAt, c.ChargeID)
	return err
}

const chargeCols = `charge_id, customer_id, period, payment_method_id, amount, currency, status,
	attempts, payment_intent_id, last_error, next_retry_at, updated_at`

func scanCharge(row interface{ Scan(...any) error }) (Charge, error) {
	var c Charge
	err := row.Scan(&c.ChargeID, &c.CustomerID, &c.Period, &c.PaymentMethodID, &c.Amount, &c.Currency, &c.Status,
		&c.Attempts, &c.PaymentIntentID, &c.LastError, &c.NextRetryAt, &c.UpdatedAt)
	return c, err
}

func (s *Store) GetCharge(ctx context.Context, id string) (*Charge, error) {
	c, err := scanCharge(s.db.QueryRowContext(ctx, `SELECT `+chargeCols+` FROM charges WHERE charge_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("charge %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ChargesByStatus lists charges with the given status.
func (s *Store) ChargesByStatus(ctx context.Context, st Status) ([]Charge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+chargeCols+` FROM charges WHERE status = ?`, st)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Charge
	for rows.Next() {
		c, err := scanCharge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
