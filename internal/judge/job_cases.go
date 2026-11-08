package judge

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"

	"golang.org/x/sync/errgroup"

	"github.com/My-Online-Judge/judge-server/internal/bundle"
	"github.com/My-Online-Judge/judge-server/internal/compare"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// runCases runs every case through a pool of PoolSize(budget, reservation)
// workers and returns the results in bundle order. The first failure (an internal
// error, or ctx ending) cancels the cases still running and is what comes back.
func (j *job) runCases(ctx context.Context) ([]CaseResult, error) {
	r := j.req
	reservation := Reservation(r.MaxMemory, r.Lang.Run.MemoryLimitCheckOnly)
	j.pool = PoolSize(j.e.d.Cfg.RunPoolBudget, reservation)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(j.pool)
	cases := r.Bundle.Cases
	results := make([]CaseResult, len(cases))
	for i := range cases {
		if gctx.Err() != nil {
			break // cancelled or failed: start nothing more
		}
		g.Go(func() error {
			res, err := j.runCase(gctx, i, cases[i])
			results[i] = res
			return err
		})
	}
	err := g.Wait()
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if err != nil {
		return nil, err
	}
	return results, nil
}

// runCase runs case i (0-based; its files are named by the 1-based position, so
// no case name ever reaches a path) and judges it: derive the result from the
// run, then compare the output by md5 or ask the checker.
func (j *job) runCase(ctx context.Context, i int, c bundle.Case) (CaseResult, error) {
	if err := ctx.Err(); err != nil {
		return CaseResult{}, err
	}
	r := j.req
	cfg := j.e.d.Cfg
	n := strconv.Itoa(i + 1)

	in := c.InputPath
	if in == "" { // an inline case
		in = filepath.Join(j.inDir, n+".in")
		if err := writeNewGroup(in, c.InputBytes, 0o640, int(j.gid)); err != nil {
			return CaseResult{}, fmt.Errorf("write input of case %s: %w", c.Name, err)
		}
	}
	outPath := filepath.Join(j.outDir, n+".out")
	errPath := filepath.Join(j.outDir, n+".err")

	res, err := j.runProgram(ctx, c.Name, in, outPath, errPath)
	if err != nil {
		return CaseResult{}, err
	}
	cr := CaseResult{
		Name:        c.Name,
		Error:       int32(res.Error),
		CPUTimeMs:   res.CPUTimeMs,
		RealTimeMs:  res.RealTimeMs,
		MemoryBytes: res.MemoryBytes,
		Signal:      int32(res.Signal),
		ExitCode:    int32(res.ExitCode),
	}
	cr.Result = Derive(res, Limits{
		MaxCPUTimeMs:   r.MaxCPUTimeMs,
		MaxRealTimeMs:  timeFactor * r.MaxCPUTimeMs,
		MaxMemoryBytes: r.MaxMemory,
	})

	// The md5 is only compared for a run that ended cleanly and has no checker;
	// the response output is wanted whatever the run did, once the program ran.
	compareMD5 := cr.Result == codeOK && r.Checker == nil
	wantOutput := r.Output && res.Error == 0
	if compareMD5 || wantOutput {
		limit := max(cfg.OutputResponseCap, 0) + 1
		if compareMD5 {
			limit = cfg.OutputFileCap + 1
		}
		data, err := readHead(outPath, limit)
		if err != nil {
			return CaseResult{}, fmt.Errorf("read output of case %s: %w", c.Name, err)
		}
		if compareMD5 {
			if int64(len(data)) > cfg.OutputFileCap {
				// RLIMIT_FSIZE keeps a program under the cap, so this is the sandbox failing.
				return CaseResult{}, fmt.Errorf("output of case %s is over the %d byte file cap", c.Name, cfg.OutputFileCap)
			}
			var ok bool
			if cr.OutputMD5, ok = compare.Matches(data, c.StrippedOutputMD5); !ok {
				cr.Result = codeWrongAnswer
			}
		}
		if wantOutput {
			if int64(len(data)) > cfg.OutputResponseCap {
				data, cr.OutputTruncated = data[:max(cfg.OutputResponseCap, 0)], true
			}
			cr.Output = slices.Clone(data)
		}
	}

	if cr.Result == codeOK && r.Checker != nil {
		if cr.Result, cr.Error, err = j.runChecker(ctx, n, c.Name, in, outPath); err != nil {
			return CaseResult{}, err
		}
	}
	return cr, nil
}

// runProgram runs the submission on one case. It holds the case's reservation
// from the shared budget for the run only, so the checker's own reservation is
// never requested while this one is held.
func (j *job) runProgram(ctx context.Context, name, in, outPath, errPath string) (sandbox.Result, error) {
	r := j.req
	cfg := j.e.d.Cfg
	checkOnly := r.Lang.Run.MemoryLimitCheckOnly

	v := j.vars()
	v.InFilePath, v.UserOutFilePath = in, outPath
	argv, err := Split(Expand(r.Lang.Run.Command, v))
	if err != nil {
		return sandbox.Result{}, invalidf("run command: %v", err)
	}

	release, err := Acquire(ctx, j.e.d.Budget, Reservation(r.MaxMemory, checkOnly), cfg.RunPoolBudget)
	if err != nil {
		return sandbox.Result{}, err
	}
	defer release()

	var addressSpace int64 // RLIMIT_AS is 2 x the limit, unless only the limit check is wanted
	if !checkOnly {
		addressSpace = Reservation(r.MaxMemory, false)
	}
	res, err := j.e.d.Runner(ctx, sandbox.Spec{
		Argv:            argv,
		Env:             j.env(r.Lang.Run.Env, false),
		Dir:             j.srcDir,
		UID:             j.slotID(),
		GID:             j.slotID(),
		Stdin:           in,
		Stdout:          outPath,
		Stderr:          errPath,
		MaxCPUTimeMs:    r.MaxCPUTimeMs,
		MaxRealTimeMs:   timeFactor * r.MaxCPUTimeMs,
		MaxAddressSpace: addressSpace,
		MaxStack:        maxStack,
		MaxOutputBytes:  cfg.OutputFileCap,
		MaxProcs:        maxProcs,
		Seccomp:         r.Lang.Run.SeccompRule,
	})
	j.sweep("run of case " + name)
	if err != nil {
		return sandbox.Result{}, runnerFailure(ctx, "run case "+name, err)
	}
	return res, nil
}
