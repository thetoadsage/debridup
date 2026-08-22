package main

import (
	"testing"
	"time"
)

func TestParseMaxConcurrentChecks(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "default", want: 4},
		{name: "minimum", value: "1", want: 1},
		{name: "maximum", value: "32", want: 32},
		{name: "zero", value: "0", wantErr: true},
		{name: "above maximum", value: "33", wantErr: true},
		{name: "not an integer", value: "many", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseMaxConcurrentChecks(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseMaxConcurrentChecks(%q) error = %v, wantErr %v", test.value, err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("parseMaxConcurrentChecks(%q) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

func TestRunCoordinatorRejectsOverlap(t *testing.T) {
	c := newRunCoordinator(2)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := c.Claim(1, now, 15*time.Second); got != claimAccepted {
		t.Fatalf("first claim = %v, want accepted", got)
	}
	if got := c.Claim(1, now.Add(20*time.Second), 15*time.Second); got != claimOverlap {
		t.Fatalf("overlapping claim = %v, want overlap", got)
	}
	c.Release(1)
	if got := c.Claim(1, now.Add(20*time.Second), 15*time.Second); got != claimNotDue {
		t.Fatalf("claim immediately after overlap release = %v, want not due", got)
	}
	if got := c.Claim(1, now.Add(35*time.Second), 15*time.Second); got != claimAccepted {
		t.Fatalf("claim one interval after overlap = %v, want accepted", got)
	}
}

func TestRunCoordinatorBoundsGlobalConcurrency(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := c.Claim(1, now, time.Second); got != claimAccepted {
		t.Fatalf("first claim = %v, want accepted", got)
	}
	if got := c.Claim(2, now, time.Second); got != claimCapacity {
		t.Fatalf("second claim = %v, want capacity", got)
	}
}

func TestRunCoordinatorForgetsLastRun(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := c.Claim(1, now, time.Hour); got != claimAccepted {
		t.Fatalf("first claim = %v, want accepted", got)
	}
	c.Release(1)
	c.Forget(1)
	if got := c.Claim(1, now.Add(time.Second), time.Hour); got != claimAccepted {
		t.Fatalf("claim after forget = %v, want accepted", got)
	}
}

func TestRunCoordinatorDuplicateReleasePreservesBound(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := c.Claim(1, now, time.Minute); got != claimAccepted {
		t.Fatalf("first claim = %v, want accepted", got)
	}
	c.Release(1)
	c.Release(1)
	if got := c.Claim(2, now, time.Minute); got != claimAccepted {
		t.Fatalf("claim after release = %v, want accepted", got)
	}
	if got := c.Claim(3, now, time.Minute); got != claimCapacity {
		t.Fatalf("claim after duplicate release = %v, want capacity", got)
	}
}

func TestRunCoordinatorRestoresCapacityOnRelease(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := c.Claim(1, now, time.Minute); got != claimAccepted {
		t.Fatalf("first claim = %v, want accepted", got)
	}
	if got := c.Claim(2, now, time.Minute); got != claimCapacity {
		t.Fatalf("claim at capacity = %v, want capacity", got)
	}
	c.Release(1)
	if got := c.Claim(2, now.Add(time.Second), time.Minute); got != claimAccepted {
		t.Fatalf("claim after release = %v, want accepted", got)
	}
}

func TestRunCoordinatorDueOverlapAdvancesScheduleAndMetric(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := c.Claim(1, now, time.Minute); got != claimAccepted {
		t.Fatalf("first claim = %v, want accepted", got)
	}
	if got := c.Claim(1, now.Add(time.Minute), time.Minute); got != claimOverlap {
		t.Fatalf("due active claim = %v, want overlap", got)
	}
	c.Release(1)
	if got := c.Claim(1, now.Add(61*time.Second), time.Minute); got != claimNotDue {
		t.Fatalf("post-overlap claim = %v, want not due", got)
	}
	if got := c.SkippedOverlaps(); got != 1 {
		t.Fatalf("skipped overlaps = %d, want 1", got)
	}
}

func TestRunCoordinatorRetriesPendingImmediateAfterCapacity(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := c.Claim(1, now, time.Hour); got != claimAccepted {
		t.Fatalf("first claim = %v, want accepted", got)
	}
	c.RequestImmediate(2)
	if got := c.Claim(2, now, time.Hour); got != claimCapacity {
		t.Fatalf("pending claim at capacity = %v, want capacity", got)
	}
	c.Release(1)
	if got := c.Claim(2, now.Add(time.Second), time.Hour); got != claimAccepted {
		t.Fatalf("retried pending claim = %v, want accepted", got)
	}
}

func TestRunCoordinatorOverdueRunDoesNotStarveLaterMonitor(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := c.Claim(1, now, time.Minute); got != claimAccepted {
		t.Fatalf("first monitor claim = %v, want accepted", got)
	}
	if got := c.Claim(1, now.Add(time.Minute), time.Minute); got != claimOverlap {
		t.Fatalf("overdue active claim = %v, want overlap", got)
	}
	if got := c.Claim(2, now.Add(time.Minute), time.Minute); got != claimCapacity {
		t.Fatalf("later monitor while active = %v, want capacity", got)
	}
	c.Release(1)
	if got := c.Claim(1, now.Add(65*time.Second), time.Minute); got != claimNotDue {
		t.Fatalf("earlier monitor after release = %v, want not due", got)
	}
	if got := c.Claim(2, now.Add(65*time.Second), time.Minute); got != claimAccepted {
		t.Fatalf("later monitor after release = %v, want accepted", got)
	}
}

func TestRunCoordinatorCompletedManualRunDelaysScheduledRun(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := c.ClaimManual(1, now); got != claimAccepted {
		t.Fatalf("manual claim = %v, want accepted", got)
	}
	c.Release(1)
	if got := c.Claim(1, now.Add(5*time.Second), time.Minute); got != claimNotDue {
		t.Fatalf("next scheduler tick = %v, want not due", got)
	}
	if got := c.Claim(1, now.Add(time.Minute), time.Minute); got != claimAccepted {
		t.Fatalf("claim after interval = %v, want accepted", got)
	}
}
