// Package testutil holds the fixtures the judge-server tests share: a scripted
// stand-in for the sandbox runner and an on-disk test-case bundle builder.
package testutil

import (
	"context"
	"os"
	"slices"
	"sync"
	"syscall"

	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// Step is what FakeRunner answers for one call.
type Step struct {
	Result sandbox.Result
	Err    error // the runner's own failure, as sandbox.Run returns it

	// Stdout and Stderr are written to Spec.Stdout and Spec.Stderr when they are
	// not nil and the path is not "" (the shim would have created the files).
	// When both paths are the same string the shim opens the file once and shares
	// it between the two streams (2>&1), so nothing is overwritten; the fake
	// mirrors that by writing the whole Stdout and then the whole Stderr into the
	// one file. It cannot interleave real writes.
	Stdout, Stderr []byte

	// Files are created (mode 0755, parent directories must exist) before the
	// answer: the artifact a compile would have produced.
	Files map[string][]byte

	// Inspect is called with the Spec first, before anything else.
	Inspect func(sandbox.Spec)

	// Block, when not nil, holds the call until it is closed or the call's ctx is
	// done. A call ended by ctx answers like sandbox.Run: SIGKILL and ctx.Err().
	Block <-chan struct{}
}

type script struct {
	steps []Step
	fn    func(sandbox.Spec) Step
	n     int
}

// FakeRunner stands in for the sandbox runner (Deps.Runner): it answers each
// call from a script keyed by Argv[0] and records every Spec it is given. A call
// whose Argv[0] has no script succeeds with a zero Result and writes nothing.
// It is safe for concurrent use.
type FakeRunner struct {
	mu       sync.Mutex
	calls    []sandbox.Spec
	scripts  map[string]*script
	inFlight map[string]int
	peak     map[string]int
	ctxDone  int
}

// NewFakeRunner returns a FakeRunner with no scripts.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{
		scripts:  map[string]*script{},
		inFlight: map[string]int{},
		peak:     map[string]int{},
	}
}

// Script queues steps for calls whose Argv[0] is argv0: call i gets steps[i], and
// the last step answers every call after the queue runs out.
func (f *FakeRunner) Script(argv0 string, steps ...Step) *FakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[argv0] = &script{steps: steps}
	return f
}

// Handle answers every call whose Argv[0] is argv0 with what fn returns for that
// call's Spec, so a concurrent caller can get a different answer per case.
func (f *FakeRunner) Handle(argv0 string, fn func(sandbox.Spec) Step) *FakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[argv0] = &script{fn: fn}
	return f
}

// Run has the signature of Deps.Runner.
func (f *FakeRunner) Run(ctx context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	// Like sandbox.Run, nothing starts under a context that is already done.
	if err := ctx.Err(); err != nil {
		return sandbox.Result{}, err
	}
	key := ""
	if len(spec.Argv) > 0 {
		key = spec.Argv[0]
	}

	f.mu.Lock()
	f.calls = append(f.calls, cloneSpec(spec))
	var step Step
	var fn func(sandbox.Spec) Step
	if s := f.scripts[key]; s != nil {
		switch {
		case s.fn != nil:
			fn = s.fn
		case len(s.steps) > 0:
			step = s.steps[min(s.n, len(s.steps)-1)]
			s.n++
		}
	}
	f.inFlight[key]++
	f.peak[key] = max(f.peak[key], f.inFlight[key])
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight[key]--
		f.mu.Unlock()
	}()

	if fn != nil {
		step = fn(cloneSpec(spec))
	}
	if step.Inspect != nil {
		step.Inspect(cloneSpec(spec))
	}
	if step.Block != nil {
		select {
		case <-step.Block:
		case <-ctx.Done():
			f.mu.Lock()
			f.ctxDone++
			f.mu.Unlock()
			return sandbox.Result{Signal: int(syscall.SIGKILL)}, ctx.Err()
		}
	}
	if err := writeStreams(spec, step); err != nil {
		return sandbox.Result{}, err
	}
	for path, data := range step.Files {
		if err := os.WriteFile(path, data, 0o755); err != nil {
			return sandbox.Result{}, err
		}
	}
	return step.Result, step.Err
}

// Calls returns every Spec the runner was given, in call order.
func (f *FakeRunner) Calls() []sandbox.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallsFor returns the Specs whose Argv[0] is argv0, in call order.
func (f *FakeRunner) CallsFor(argv0 string) []sandbox.Spec {
	var out []sandbox.Spec
	for _, s := range f.Calls() {
		if len(s.Argv) > 0 && s.Argv[0] == argv0 {
			out = append(out, s)
		}
	}
	return out
}

// InFlight is how many calls with Argv[0] == argv0 have started and not answered.
func (f *FakeRunner) InFlight(argv0 string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight[argv0]
}

// Peak is the most calls with Argv[0] == argv0 that were in flight at once.
func (f *FakeRunner) Peak(argv0 string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak[argv0]
}

// CtxDone is how many calls were held by a Block and ended by their ctx.
func (f *FakeRunner) CtxDone() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctxDone
}

func writeStreams(spec sandbox.Spec, step Step) error {
	if spec.Stdout != "" && spec.Stdout == spec.Stderr {
		if step.Stdout == nil && step.Stderr == nil {
			return nil
		}
		return os.WriteFile(spec.Stdout, append(slices.Clone(step.Stdout), step.Stderr...), 0o644)
	}
	if step.Stdout != nil && spec.Stdout != "" {
		if err := os.WriteFile(spec.Stdout, step.Stdout, 0o644); err != nil {
			return err
		}
	}
	if step.Stderr != nil && spec.Stderr != "" {
		return os.WriteFile(spec.Stderr, step.Stderr, 0o644)
	}
	return nil
}

func cloneSpec(s sandbox.Spec) sandbox.Spec {
	s.Argv = slices.Clone(s.Argv)
	s.Env = slices.Clone(s.Env)
	s.Groups = slices.Clone(s.Groups)
	return s
}
