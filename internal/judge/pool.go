package judge

import (
	"context"
	"errors"
	"math"
	"sync"

	"golang.org/x/sync/semaphore"
)

const (
	// checkOnlySlack is what a memory_limit_check_only case reserves on top of
	// its limit (spec §6): it gets no address-space cap, so the limit alone is
	// not what it can touch.
	checkOnlySlack = int64(256) << 20

	// maxPoolSize is the most cases one job runs at once.
	maxPoolSize = 8
)

// Budget is the memory the sandbox hands out to running programs, in bytes. One
// Budget is shared by every job, so concurrent jobs cannot together exceed it.
type Budget = *semaphore.Weighted

// NewBudget returns a Budget of the given size.
func NewBudget(bytes int64) Budget { return semaphore.NewWeighted(bytes) }

// Reservation is the memory one case reserves from the Budget: twice its memory
// limit (the RLIMIT_AS the run gets), or the limit plus 256 MiB when only the
// limit check is wanted. It saturates at math.MaxInt64 instead of wrapping.
func Reservation(maxMemory int64, checkOnly bool) int64 {
	if checkOnly {
		if maxMemory > math.MaxInt64-checkOnlySlack {
			return math.MaxInt64
		}
		return maxMemory + checkOnlySlack
	}
	if maxMemory > math.MaxInt64/2 {
		return math.MaxInt64
	}
	return 2 * maxMemory
}

// PoolSize is how many cases of the given reservation fit in budget at once,
// at least 1 (a case bigger than the budget still runs, alone) and at most 8.
func PoolSize(budget, reservation int64) int {
	if reservation <= 0 {
		return 1
	}
	return int(min(max(budget/reservation, 1), maxPoolSize))
}

// CompileReservation is the memory a compile reserves from the Budget: the whole
// budget when the request sets no compile limit (compileMax <= 0, how Java and
// Go ask), else compileMax cut to capBytes.
func CompileReservation(compileMax, capBytes, budget int64) int64 {
	if compileMax <= 0 {
		return budget
	}
	return min(compileMax, capBytes)
}

var errNoBudget = errors.New("judge: memory budget must be positive")

// Acquire waits until n bytes are free in b and returns the func that gives them
// back. n is clamped to [1, budget], so a request bigger than the whole budget
// takes all of it instead of waiting forever; budget must be the size b was
// created with. release gives back the clamped amount and is safe to call more
// than once. On error (ctx done while waiting, or budget < 1) nothing is held
// and release is nil.
func Acquire(ctx context.Context, b Budget, n, budget int64) (release func(), err error) {
	if budget < 1 {
		return nil, errNoBudget
	}
	n = max(min(n, budget), 1)
	if err := b.Acquire(ctx, n); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { b.Release(n) }) }, nil
}
