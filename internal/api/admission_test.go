package api

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// settleBound bounds every wait in this file. It is far above what any of the
// awaited events needs, so a miss means a real hang, never a slow machine.
const settleBound = 2 * time.Second

// acquireResult is what one Acquire call returned.
type acquireResult struct {
	release func()
	err     error
}

// acquireAsync runs Acquire on its own goroutine and delivers its result.
func acquireAsync(a *Admission, ctx context.Context) <-chan acquireResult {
	out := make(chan acquireResult, 1)
	go func() {
		release, err := a.Acquire(ctx)
		out <- acquireResult{release: release, err: err}
	}()
	return out
}

// await returns the result of acquireAsync, failing the test if it does not arrive.
func await(t *testing.T, ch <-chan acquireResult, what string) acquireResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(settleBound):
		t.Fatalf("%s: Acquire did not return within %v", what, settleBound)
		return acquireResult{}
	}
}

// mustAcquire acquires a slot that must be free, or fails the test.
func mustAcquire(t *testing.T, a *Admission) func() {
	t.Helper()
	r := await(t, acquireAsync(a, context.Background()), "free slot")
	if r.err != nil {
		t.Fatalf("Acquire = %v, want a free slot", r.err)
	}
	return r.release
}

// waitForWaiting blocks until the queue holds n waiters.
func waitForWaiting(t *testing.T, a *Admission, n int) {
	t.Helper()
	deadline := time.Now().Add(settleBound)
	for a.Waiting() != n {
		if time.Now().After(deadline) {
			t.Fatalf("Waiting() = %d, want %d", a.Waiting(), n)
		}
		runtime.Gosched()
	}
}

// expectCounts asserts the running and waiting counters.
func expectCounts(t *testing.T, a *Admission, running, waiting int) {
	t.Helper()
	if got := a.Running(); got != running {
		t.Errorf("Running() = %d, want %d", got, running)
	}
	if got := a.Waiting(); got != waiting {
		t.Errorf("Waiting() = %d, want %d", got, waiting)
	}
}

func TestAdmissionRunsUpToMax(t *testing.T) {
	a := NewAdmission(2, 0)
	if a.Max() != 2 {
		t.Fatalf("Max() = %d, want 2", a.Max())
	}
	mustAcquire(t, a)
	mustAcquire(t, a)
	expectCounts(t, a, 2, 0)
}

func TestAdmissionQueuesThenBusy(t *testing.T) {
	a := NewAdmission(2, 1)
	mustAcquire(t, a)
	mustAcquire(t, a)

	third := acquireAsync(a, context.Background())
	waitForWaiting(t, a, 1)
	select {
	case r := <-third:
		t.Fatalf("third Acquire returned (err=%v) while both slots are taken", r.err)
	default:
	}

	// Running and queue are both full: the fourth call is refused, not queued.
	fourth := await(t, acquireAsync(a, context.Background()), "fourth")
	if !errors.Is(fourth.err, ErrBusy) {
		t.Fatalf("fourth Acquire err = %v, want ErrBusy", fourth.err)
	}
	if fourth.release != nil {
		t.Error("a refused Acquire must not return a release func")
	}
	expectCounts(t, a, 2, 1)
}

func TestAdmissionBusyWithoutQueue(t *testing.T) {
	a := NewAdmission(1, 0)
	mustAcquire(t, a)

	r := await(t, acquireAsync(a, context.Background()), "second")
	if !errors.Is(r.err, ErrBusy) {
		t.Fatalf("Acquire err = %v, want ErrBusy with queue 0", r.err)
	}
	expectCounts(t, a, 1, 0)
}

func TestAdmissionReleaseAdmitsWaiter(t *testing.T) {
	a := NewAdmission(1, 1)
	releaseFirst := mustAcquire(t, a)

	waiter := acquireAsync(a, context.Background())
	waitForWaiting(t, a, 1)

	releaseFirst()
	r := await(t, waiter, "waiter")
	if r.err != nil {
		t.Fatalf("waiter Acquire = %v, want admitted", r.err)
	}
	// The slot moved to the waiter without ever being free.
	expectCounts(t, a, 1, 0)

	r.release()
	expectCounts(t, a, 0, 0)
}

func TestAdmissionReleaseWithoutWaitersFreesSlot(t *testing.T) {
	a := NewAdmission(1, 1)
	release := mustAcquire(t, a)
	release()
	expectCounts(t, a, 0, 0)
	mustAcquire(t, a)
	expectCounts(t, a, 1, 0)
}

func TestAdmissionReleaseIsIdempotent(t *testing.T) {
	a := NewAdmission(1, 0)
	release := mustAcquire(t, a)
	release()
	next := mustAcquire(t, a)

	// A second call of the stale release must not free the slot `next` holds.
	release()
	expectCounts(t, a, 1, 0)
	if r := await(t, acquireAsync(a, context.Background()), "third"); !errors.Is(r.err, ErrBusy) {
		t.Fatalf("Acquire err = %v, want ErrBusy while next still holds the slot", r.err)
	}

	next()
	next()
	expectCounts(t, a, 0, 0)
}

func TestAdmissionCancelWhileWaiting(t *testing.T) {
	a := NewAdmission(1, 1)
	releaseFirst := mustAcquire(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	waiter := acquireAsync(a, ctx)
	waitForWaiting(t, a, 1)

	cancel()
	r := await(t, waiter, "cancelled waiter")
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Acquire err = %v, want context.Canceled", r.err)
	}
	if r.release != nil {
		t.Error("a cancelled Acquire must not return a release func")
	}
	waitForWaiting(t, a, 0)
	expectCounts(t, a, 1, 0)

	// The cancelled waiter is gone from the queue: the freed slot is not handed to it.
	releaseFirst()
	expectCounts(t, a, 0, 0)
}

func TestAdmissionDeadlineWhileWaiting(t *testing.T) {
	a := NewAdmission(1, 1)
	mustAcquire(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	r := await(t, acquireAsync(a, ctx), "waiter with deadline")
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("Acquire err = %v, want context.DeadlineExceeded", r.err)
	}
	expectCounts(t, a, 1, 0)
}

func TestAdmissionCancelledContextIsNotAdmitted(t *testing.T) {
	a := NewAdmission(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Even with a free slot, a caller that is already gone must not take it.
	r := await(t, acquireAsync(a, ctx), "cancelled caller")
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Acquire err = %v, want context.Canceled", r.err)
	}
	expectCounts(t, a, 0, 0)
}

func TestAdmissionCancelMiddleWaiterKeepsOrder(t *testing.T) {
	a := NewAdmission(1, 3)
	releaseHolder := mustAcquire(t, a)

	first := acquireAsync(a, context.Background())
	waitForWaiting(t, a, 1)
	ctx, cancel := context.WithCancel(context.Background())
	middle := acquireAsync(a, ctx)
	waitForWaiting(t, a, 2)
	last := acquireAsync(a, context.Background())
	waitForWaiting(t, a, 3)

	cancel()
	if r := await(t, middle, "middle"); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("middle Acquire err = %v, want context.Canceled", r.err)
	}
	waitForWaiting(t, a, 2)

	releaseHolder()
	r1 := await(t, first, "first")
	if r1.err != nil {
		t.Fatalf("first Acquire = %v, want admitted", r1.err)
	}
	expectCounts(t, a, 1, 1)
	r1.release()
	r3 := await(t, last, "last")
	if r3.err != nil {
		t.Fatalf("last Acquire = %v, want admitted", r3.err)
	}
	r3.release()
	expectCounts(t, a, 0, 0)
}

func TestAdmissionFIFO(t *testing.T) {
	a := NewAdmission(1, 2)
	releaseHolder := mustAcquire(t, a)

	first := acquireAsync(a, context.Background())
	waitForWaiting(t, a, 1)
	second := acquireAsync(a, context.Background())
	waitForWaiting(t, a, 2)

	releaseHolder()
	r1 := await(t, first, "first waiter")
	if r1.err != nil {
		t.Fatalf("first waiter Acquire = %v, want admitted", r1.err)
	}
	// The second waiter cannot be admitted before the first one releases.
	expectCounts(t, a, 1, 1)
	select {
	case r := <-second:
		t.Fatalf("second waiter admitted (err=%v) before the first released", r.err)
	default:
	}

	r1.release()
	r2 := await(t, second, "second waiter")
	if r2.err != nil {
		t.Fatalf("second waiter Acquire = %v, want admitted", r2.err)
	}
	expectCounts(t, a, 1, 0)
	r2.release()
	expectCounts(t, a, 0, 0)
}

// A waiter that gives up in the instant after release handed it a slot must
// pass that slot on, not keep it: the next waiter is admitted, or the slot is freed.
func TestAdmissionAbandonedHandOffGoesToNextWaiter(t *testing.T) {
	a := NewAdmission(1, 2)
	releaseHolder := mustAcquire(t, a)

	// Stand-in for a waiter whose Acquire already lost the race to its ctx.
	abandoned := make(chan struct{}, 1)
	a.mu.Lock()
	a.waiters = append(a.waiters, abandoned)
	a.mu.Unlock()
	next := acquireAsync(a, context.Background())
	waitForWaiting(t, a, 2)

	releaseHolder()
	select {
	case <-abandoned:
	default:
		t.Fatal("release did not hand the slot to the head waiter")
	}
	a.abandon(abandoned)

	r := await(t, next, "next waiter")
	if r.err != nil {
		t.Fatalf("next waiter Acquire = %v, want the abandoned slot", r.err)
	}
	expectCounts(t, a, 1, 0)
	r.release()
	expectCounts(t, a, 0, 0)
}

func TestAdmissionAbandonedHandOffWithoutWaitersFreesSlot(t *testing.T) {
	a := NewAdmission(1, 1)
	releaseHolder := mustAcquire(t, a)

	abandoned := make(chan struct{}, 1)
	a.mu.Lock()
	a.waiters = append(a.waiters, abandoned)
	a.mu.Unlock()

	releaseHolder()
	a.abandon(abandoned)
	expectCounts(t, a, 0, 0)
	mustAcquire(t, a)
}

// Cancel and release race on every iteration: whichever wins, no slot may leak
// and no counter may go negative.
func TestAdmissionCancelRacingReleaseNeverLeaksSlot(t *testing.T) {
	for i := 0; i < 500; i++ {
		a := NewAdmission(1, 1)
		releaseHolder := mustAcquire(t, a)

		ctx, cancel := context.WithCancel(context.Background())
		waiter := acquireAsync(a, ctx)
		waitForWaiting(t, a, 1)

		raced := make(chan struct{}, 2)
		go func() { cancel(); raced <- struct{}{} }()
		go func() { releaseHolder(); raced <- struct{}{} }()
		<-raced
		<-raced

		r := await(t, waiter, "racing waiter")
		if r.err == nil {
			r.release()
		} else if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("iteration %d: Acquire err = %v, want nil or context.Canceled", i, r.err)
		}
		if a.Running() != 0 || a.Waiting() != 0 {
			t.Fatalf("iteration %d: Running()=%d Waiting()=%d, want 0/0 (slot leaked)", i, a.Running(), a.Waiting())
		}
		mustAcquire(t, a)
	}
}

func TestNewAdmissionRejectsInvalidSizes(t *testing.T) {
	for _, tc := range []struct {
		name              string
		maxJobs, maxQueue int
	}{
		{"zero max jobs", 0, 0},
		{"negative max jobs", -1, 1},
		{"negative queue", 1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("NewAdmission(%d, %d) did not panic", tc.maxJobs, tc.maxQueue)
				}
			}()
			NewAdmission(tc.maxJobs, tc.maxQueue)
		})
	}
}
