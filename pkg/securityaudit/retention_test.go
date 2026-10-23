package securityaudit

import (
	"context"
	"errors"
	"testing"
	"time"
)

type retentionFake struct {
	fn func(context.Context, string, time.Time, int64, int) (int64, error)
}

func (s retentionFake) EnforceRetention(ctx context.Context, ns string, t time.Time, keep int64, batch int) (int64, error) {
	return s.fn(ctx, ns, t, keep, batch)
}
func TestRetentionConfigurationBoundsAndRedaction(t *testing.T) {
	p, err := ParseRetention("", "")
	if err != nil || p != DefaultRetention() {
		t.Fatal(p, err)
	}
	for _, input := range [][2]string{{"0", "1000"}, {"366", "1000"}, {"3.14", "1000"}, {"30", "999"}, {"30", "100000001"}, {"30", "private-mistyped-value"}} {
		if _, err := ParseRetention(input[0], input[1]); err != ErrInvalid {
			t.Fatal("unsafe retention accepted or raw error returned", err)
		}
	}
}
func TestRetentionSweepBudgetAndBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	calls := 0
	f := retentionFake{func(ctx context.Context, ns string, cutoff time.Time, keep int64, batch int) (int64, error) {
		calls++
		if ns != "kyber-system" || !cutoff.Equal(now.Add(-30*24*time.Hour)) || keep != 1_000_000 || batch != 1000 {
			t.Fatal("retention policy not honored")
		}
		return 1000, nil
	}}
	n, err := SweepRetention(context.Background(), f, "kyber-system", DefaultRetention(), now)
	if err != nil || n != 100_000 || calls != 100 {
		t.Fatal("unbounded sweep", n, calls, err)
	}
	f.fn = func(context.Context, string, time.Time, int64, int) (int64, error) { return 0, ErrUnavailable }
	if _, err := SweepRetention(context.Background(), f, "kyber-system", DefaultRetention(), now); !errors.Is(err, ErrUnavailable) {
		t.Fatal("failed cleanup reported success", err)
	}
}
func TestRetentionStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	f := retentionFake{func(context.Context, string, time.Time, int64, int) (int64, error) { calls++; cancel(); return 0, nil }}
	if err := RunRetention(ctx, f, "kyber-system", DefaultRetention()); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("retention did not stop", calls, err)
	}
	calls = 0
	if _, err := SweepRetention(ctx, f, "kyber-system", DefaultRetention(), time.Now()); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("canceled sweep reached store", err)
	}
}
