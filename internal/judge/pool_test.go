package judge

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

func TestReservation(t *testing.T) {
	tests := []struct {
		name      string
		maxMemory int64
		checkOnly bool
		want      int64
	}{
		{"address-space limit is twice the memory limit", 128 * mib, false, 256 * mib},
		{"check-only adds 256 MiB of slack", 256 * mib, true, 512 * mib},
		{"huge limit saturates instead of wrapping", math.MaxInt64, false, math.MaxInt64},
		{"huge check-only limit saturates instead of wrapping", math.MaxInt64 - mib, true, math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Reservation(tt.maxMemory, tt.checkOnly); got != tt.want {
				t.Errorf("Reservation(%d, %v) = %d, want %d", tt.maxMemory, tt.checkOnly, got, tt.want)
			}
		})
	}
}

func TestPoolSize(t *testing.T) {
	tests := []struct {
		name            string
		maxMemory       int64
		checkOnly       bool
		budget          int64
		reservationOver int64 // when non-zero, used instead of Reservation(...)
		want            int
	}{
		{name: "128 MiB cases in 1 GiB", maxMemory: 128 * mib, budget: gib, want: 4},
		{name: "64 MiB cases hit the cap of 8", maxMemory: 64 * mib, budget: gib, want: 8},
		{name: "check-only 256 MiB cases", maxMemory: 256 * mib, checkOnly: true, budget: gib, want: 2},
		{name: "1 GiB case still gets one worker", maxMemory: gib, budget: gib, want: 1},
		{name: "reservation above the budget still gets one worker", maxMemory: 4 * gib, budget: gib, want: 1},
		{name: "tiny reservation is capped at 8", maxMemory: 1, budget: gib, want: 8},
		{name: "zero reservation gives 1", reservationOver: 0, budget: gib, want: 1},
		{name: "zero budget gives 1", maxMemory: 128 * mib, budget: 0, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := tt.reservationOver
			if tt.maxMemory != 0 {
				res = Reservation(tt.maxMemory, tt.checkOnly)
			}
			if got := PoolSize(tt.budget, res); got != tt.want {
				t.Errorf("PoolSize(%d, %d) = %d, want %d", tt.budget, res, got, tt.want)
			}
		})
	}
}

func TestPoolSizeNegativeReservation(t *testing.T) {
	if got := PoolSize(gib, -5); got != 1 {
		t.Errorf("PoolSize(1 GiB, -5) = %d, want 1", got)
	}
}

func TestCompileReservation(t *testing.T) {
	tests := []struct {
		name       string
		compileMax int64
		cap        int64
		want       int64
	}{
		{"-1 reserves the whole pool", -1, 512 * mib, gib},
		{"0 reserves the whole pool", 0, 512 * mib, gib},
		{"request above the cap is cut to the cap", gib, 512 * mib, 512 * mib},
		{"request below the cap is kept", 256 * mib, 512 * mib, 256 * mib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompileReservation(tt.compileMax, tt.cap, gib); got != tt.want {
				t.Errorf("CompileReservation(%d, %d, %d) = %d, want %d", tt.compileMax, tt.cap, gib, got, tt.want)
			}
		})
	}
}

func TestAcquireClampsToBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := NewBudget(gib)

	release, err := Acquire(ctx, b, 2*gib, gib)
	if err != nil {
		t.Fatalf("Acquire 2 GiB from a 1 GiB budget: %v", err)
	}
	if b.TryAcquire(1) {
		t.Fatal("budget has room left: the clamped acquire must hold the whole budget")
	}

	// A second acquire has to wait for the first release.
	got := make(chan error, 1)
	go func() {
		rel, err := Acquire(ctx, b, 512*mib, gib)
		if err == nil {
			rel()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("second Acquire returned before the release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("second Acquire after the release: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("second Acquire never returned after the release")
	}
	if !b.TryAcquire(gib) {
		t.Error("the whole budget is not free after both releases")
	}
}

func TestAcquireClampsNonPositiveToOne(t *testing.T) {
	b := NewBudget(10)
	release, err := Acquire(context.Background(), b, -7, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !b.TryAcquire(9) {
		t.Error("a non-positive n must reserve exactly 1")
	}
	if b.TryAcquire(1) {
		t.Error("budget should be full: 1 + 9 held")
	}
	release()
	if !b.TryAcquire(1) {
		t.Error("release must give back the 1 that was reserved")
	}
}

func TestAcquireReleasesTheClampedAmount(t *testing.T) {
	b := NewBudget(100)
	release, err := Acquire(context.Background(), b, 500, 100)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !b.TryAcquire(100) {
		t.Error("after release the whole budget (the clamped 100, not 500) must be free")
	}
}

func TestAcquireReleaseIsIdempotent(t *testing.T) {
	b := NewBudget(100)
	release, err := Acquire(context.Background(), b, 60, 100)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release() // cancel and cleanup paths may both release; the second must be a no-op
	if !b.TryAcquire(100) {
		t.Fatal("budget not fully free after release")
	}
	if b.TryAcquire(1) {
		t.Error("a second release gave back more than was held")
	}
}

func TestAcquireCancelledWhileWaiting(t *testing.T) {
	b := NewBudget(100)
	hold, err := Acquire(context.Background(), b, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rel, err := Acquire(ctx, b, 50, 100)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire on an exhausted budget with an expiring ctx: err = %v, want DeadlineExceeded", err)
	}
	if rel != nil {
		t.Error("a failed Acquire must not return a release func")
	}
	hold()
	if !b.TryAcquire(100) {
		t.Error("the failed Acquire leaked part of the budget")
	}
}

func TestAcquireRejectsNonPositiveBudget(t *testing.T) {
	// A zero-size semaphore can never grant anything: fail instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Acquire(ctx, NewBudget(0), 10, 0); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Acquire with budget 0: err = %v, want an immediate budget error", err)
	}
}
