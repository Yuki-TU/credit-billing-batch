package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestRetryTransient(t *testing.T) {
	transient := errors.New("dial tcp: i/o timeout")
	cases := []struct {
		name      string
		errs      []error // returned by successive calls; nil = success
		wantCalls int
		wantErr   bool
	}{
		{"resumes after two timeouts", []error{transient, transient, nil}, 3, false},
		{"gives up after attempts", []error{transient, transient, transient, transient}, 3, true},
		{"server error is not retried", []error{&mysql.MySQLError{Number: 1045, Message: "Access denied"}}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := retryTransient(context.Background(), 3, time.Millisecond, func() error {
				calls++
				return tc.errs[calls-1]
			})
			if calls != tc.wantCalls || (err != nil) != tc.wantErr {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRetryTransientStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryTransient(ctx, 5, time.Hour, func() error {
		calls++
		cancel()
		return errors.New("timeout")
	})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
