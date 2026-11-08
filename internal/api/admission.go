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
// order. Every running job holds one slot, a number in [0, maxJobs) that no other
// running job has: the engine runs the job under that slot's own uid and gid, so
// two jobs never share an identity. The zero value is not usable: build one with
// NewAdmission.
type Admission struct {
	maxJobs  int
	maxQueue int

	mu      sync.Mutex
	free    []int // the slots nobody holds, lowest first
	waiters []chan int
}

// NewAdmission returns an Admission with maxJobs slots and room for maxQueue
// waiters. It panics when maxJobs < 1 or maxQueue < 0: config validation
// rejects those values first, so reaching the panic is a programming error.
func NewAdmission(maxJobs, maxQueue int) *Admission {
	if maxJobs < 1 || maxQueue < 0 {
		panic("api: NewAdmission needs maxJobs >= 1 and maxQueue >= 0")
	}
	free := make([]int, maxJobs)
	for i := range free {
		free[i] = i
	}
	return &Admission{maxJobs: maxJobs, maxQueue: maxQueue, free: free}
}

// Acquire takes a job slot, waiting in the FIFO queue when none is free, and
// returns its number: the lowest free one, in [0, Max()). The returned release
// gives the slot back; it is safe to call more than once and only the first call
// has an effect.
//
// It returns ErrBusy at once when every slot is taken and the queue already
// holds maxQueue waiters, and ctx.Err() when ctx ends first, including a ctx
// that is already done on entry. On any error the caller holds no slot.
func (a *Admission) Acquire(ctx context.Context) (slot int, release func(), err error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}

	a.mu.Lock()
	if len(a.free) > 0 {
		slot = a.free[0]
		a.free = a.free[1:]
		a.mu.Unlock()
		return slot, a.releaser(slot), nil
	}
	if len(a.waiters) >= a.maxQueue {
		a.mu.Unlock()
		return 0, nil, ErrBusy
	}
	wake := make(chan int, 1) // buffered, so handing a slot over never blocks
	a.waiters = append(a.waiters, wake)
	a.mu.Unlock()

	select {
	case slot := <-wake:
		return slot, a.releaser(slot), nil
	case <-ctx.Done():
		a.abandon(wake)
		return 0, nil, ctx.Err()
	}
}

// Running returns the number of slots in use, those handed to a waiter that has
// not woken yet included.
func (a *Admission) Running() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.maxJobs - len(a.free)
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
func (a *Admission) releaser(slot int) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.freeLocked(slot)
		})
	}
}

// abandon removes a waiter whose ctx ended. When the waiter is no longer in
// the queue, freeLocked already handed it a slot (it sits in the waiter's
// buffered channel) that nobody will use, so the slot goes on to the next waiter
// or back to the pool.
func (a *Admission) abandon(wake chan int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := slices.Index(a.waiters, wake); i >= 0 {
		a.waiters = slices.Delete(a.waiters, i, i+1)
		return
	}
	a.freeLocked(<-wake) // the hand-over was sent under a.mu, before we took it: it is there
}

// freeLocked gives slot back: to the head waiter when there is one (the slot
// stays in use), otherwise to the pool, which keeps its free slots in order.
// Callers hold a.mu.
func (a *Admission) freeLocked(slot int) {
	if len(a.waiters) == 0 {
		i, _ := slices.BinarySearch(a.free, slot)
		a.free = slices.Insert(a.free, i, slot)
		return
	}
	head := a.waiters[0]
	a.waiters = slices.Delete(a.waiters, 0, 1)
	head <- slot
}
