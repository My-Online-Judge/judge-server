package api

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// ErrBusy is returned by Acquire when every job slot is taken and the wait
// queue is full.
var ErrBusy = errors.New("judge server busy")

// Admission limits how many jobs run at once (maxJobs) and how many callers
// may wait for a slot (maxQueue). Waiters are admitted strictly in arrival
// order. The zero value is not usable: build one with NewAdmission.
type Admission struct {
	maxJobs  int
	maxQueue int

	mu      sync.Mutex
	running int // slots in use, including slots handed to a waiter that has not woken yet
	waiters []chan struct{}
}

// NewAdmission returns an Admission with maxJobs slots and room for maxQueue
// waiters. It panics when maxJobs < 1 or maxQueue < 0: config validation
// rejects those values first, so reaching the panic is a programming error.
func NewAdmission(maxJobs, maxQueue int) *Admission {
	if maxJobs < 1 || maxQueue < 0 {
		panic("api: NewAdmission needs maxJobs >= 1 and maxQueue >= 0")
	}
	return &Admission{maxJobs: maxJobs, maxQueue: maxQueue}
}

// Acquire takes a job slot, waiting in the FIFO queue when none is free. The
// returned release gives the slot back; it is safe to call more than once and
// only the first call has an effect.
//
// It returns ErrBusy at once when running == maxJobs and the queue already
// holds maxQueue waiters, and ctx.Err() when ctx ends first, including a ctx
// that is already done on entry. On any error the caller holds no slot.
func (a *Admission) Acquire(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	a.mu.Lock()
	if a.running < a.maxJobs {
		a.running++
		a.mu.Unlock()
		return a.releaser(), nil
	}
	if len(a.waiters) >= a.maxQueue {
		a.mu.Unlock()
		return nil, ErrBusy
	}
	wake := make(chan struct{}, 1) // buffered, so handing a slot over never blocks
	a.waiters = append(a.waiters, wake)
	a.mu.Unlock()

	select {
	case <-wake:
		return a.releaser(), nil
	case <-ctx.Done():
		a.abandon(wake)
		return nil, ctx.Err()
	}
}

// Running returns the number of slots in use.
func (a *Admission) Running() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// Waiting returns the number of callers queued for a slot.
func (a *Admission) Waiting() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.waiters)
}

// Max returns the number of job slots.
func (a *Admission) Max() int {
	return a.maxJobs
}

// releaser returns the idempotent release func for one acquired slot.
func (a *Admission) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.freeLocked()
		})
	}
}

// abandon removes a waiter whose ctx ended. When the waiter is no longer in
// the queue, freeLocked already handed it a slot that nobody will use, so the
// slot goes on to the next waiter or back to the pool.
func (a *Admission) abandon(wake chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := slices.Index(a.waiters, wake); i >= 0 {
		a.waiters = slices.Delete(a.waiters, i, i+1)
		return
	}
	a.freeLocked()
}

// freeLocked gives one slot back: to the head waiter when there is one (the
// slot stays counted as running), otherwise to the pool. Callers hold a.mu.
func (a *Admission) freeLocked() {
	if len(a.waiters) == 0 {
		a.running--
		return
	}
	head := a.waiters[0]
	a.waiters = slices.Delete(a.waiters, 0, 1)
	head <- struct{}{}
}
