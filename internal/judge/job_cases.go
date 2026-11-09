package judge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sync/errgroup"

	"github.com/My-Online-Judge/judge-server/internal/bundle"
	"github.com/My-Online-Judge/judge-server/internal/compare"
	"github.com/My-Online-Judge/judge-server/internal/sandbox"
)

// runCases runs every case through a pool of PoolSize(budget, reservation)
// workers and returns the results in bundle order. Each judged case is also
// handed to the request's OnCase hook, when there is one, before its worker
// moves on. The first failure (an internal error, or ctx ending) cancels the
// cases still running and is what comes back.
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
			if err != nil {
				return err
			}
			results[i] = res
			if r.OnCase != nil {
				r.OnCase(res)
			}
			return nil
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
// run, then compare the output by md5 or ask the checker. The case's output file
// is deleted as soon as it is judged, however it ended, so a job holds at most
// one (capped at OutputFileCap) per case in flight, not one per case.
func (j *job) runCase(ctx context.Context, i int, c bundle.Case) (CaseResult, error) {
	if err := ctx.Err(); err != nil {
		return CaseResult{}, err
	}
	r := j.req
	n := strconv.Itoa(i + 1)

	in := c.InputPath
	if in == "" { // an inline case
		in = filepath.Join(j.inDir, n+".in")
		if err := writeNewGroup(in, c.InputBytes, 0o640, int(j.gid)); err != nil {
			return CaseResult{}, fmt.Errorf("write input of case %s: %w", c.Name, err)
		}
	}
	outPath := filepath.Join(j.outDir, n+".out")
	defer removeFile(outPath)

	res, err := j.runProgram(ctx, c.Name, in, outPath)
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
		got, err := j.readOutput(c.Name, outPath, compareMD5, wantOutput)
		if err != nil {
			return CaseResult{}, err
		}
		if compareMD5 {
			if cr.OutputMD5 = got.md5; cr.OutputMD5 != c.StrippedOutputMD5 {
				cr.Result = codeWrongAnswer
			}
		}
		if wantOutput {
			cr.Output, cr.OutputTruncated = got.head, got.truncated
		}
	}

	if cr.Result == codeOK && r.Checker != nil {
		if cr.Result, cr.Error, err = j.runChecker(ctx, n, c.Name, in, outPath); err != nil {
			return CaseResult{}, err
		}
	}
	return cr, nil
}

// outputRead is what readOutput took from a case's output file.
type outputRead struct {
	md5       string // md5 of the normalised content (compare.Hasher), when it was asked for
	head      []byte // the first OutputResponseCap bytes, when they were asked for
	truncated bool   // the file holds more than head
}

// readOutput reads the output file of case name once: the md5 of its normalised content
// when hash is set, and the first OutputResponseCap bytes when head is set. The file goes
// through compare.Hasher, a piece at a time, so the server never holds it (it can be as big as
// OutputFileCap): what stays in memory is the 32 KiB copy buffer, the Hasher's bounded state
// and at most OutputResponseCap+1 bytes of head. Without hash only that many bytes are read.
// A file that does not exist reads as empty.
func (j *job) readOutput(name, path string, hash, head bool) (outputRead, error) {
	cfg := j.e.d.Cfg
	var src io.Reader = eofReader{}
	switch f, err := os.Open(path); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return outputRead{}, fmt.Errorf("read output of case %s: %w", name, err)
	default:
		defer f.Close()
		src = f
	}

	headLimit := max(cfg.OutputResponseCap, 0) + 1 // one byte more than is returned, to know that the file has more
	var dst []io.Writer
	var hasher *compare.Hasher
	var first *headWriter
	limit := headLimit
	if hash {
		hasher = compare.NewHasher()
		dst = append(dst, hasher)
		limit = cfg.OutputFileCap + 1
	}
	if head {
		first = &headWriter{limit: headLimit}
		dst = append(dst, first)
	}
	n, err := io.Copy(io.MultiWriter(dst...), io.LimitReader(src, limit))
	if err != nil {
		return outputRead{}, fmt.Errorf("read output of case %s: %w", name, err)
	}
	if hash && n > cfg.OutputFileCap {
		// RLIMIT_FSIZE keeps a program under the cap, so this is the sandbox failing.
		return outputRead{}, fmt.Errorf("output of case %s is over the %d byte file cap", name, cfg.OutputFileCap)
	}

	var out outputRead
	if hash {
		out.md5 = hasher.Hex()
	}
	if head {
		out.head = first.buf
		if int64(len(out.head)) > cfg.OutputResponseCap {
			out.head, out.truncated = out.head[:max(cfg.OutputResponseCap, 0)], true
		}
	}
	return out, nil
}

// eofReader is the empty file.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// headWriter keeps the first limit bytes written to it and accepts, and drops, the rest.
type headWriter struct {
	buf   []byte
	limit int64
}

func (w *headWriter) Write(p []byte) (int, error) {
	if room := w.limit - int64(len(w.buf)); room > 0 {
		w.buf = append(w.buf, p[:min(int64(len(p)), room)]...)
	}
	return len(p), nil
}

// runProgram runs the submission on one case, stdout into outPath. Its stderr goes
// to /dev/null: it never affects the verdict (spec 4.3), and a file for it would be
// one more thing a program can fill (the compile's and the checker's stderr are still
// kept). It holds the case's reservation from the shared budget for the run only, so
// the checker's own reservation is never requested while this one is held. The slot's
// uid is swept when the run is over.
func (j *job) runProgram(ctx context.Context, name, in, outPath string) (sandbox.Result, error) {
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
	oomBefore := j.oomKills()
	res, err := j.e.d.Runner(ctx, sandbox.Spec{
		Argv:            argv,
		Env:             j.env(r.Lang.Run.Env, false),
		Dir:             j.srcDir,
		UID:             j.slotID(),
		GID:             j.slotID(),
		Stdin:           in,
		Stdout:          outPath,
		Stderr:          "", // /dev/null
		MaxCPUTimeMs:    r.MaxCPUTimeMs,
		MaxRealTimeMs:   timeFactor * r.MaxCPUTimeMs,
		MaxAddressSpace: addressSpace,
		MaxStack:        maxStack,
		MaxOutputBytes:  cfg.OutputFileCap,
		MaxProcs:        maxProcs,
		Seccomp:         r.Lang.Run.SeccompRule,
		Cgroup:          j.cgFile(),
	})
	j.sweep("run of case " + name)
	// The job's cgroup went over memory.max while this run was going: a memory limit exceeded, as the
	// shim's own measurement would say (the peak is raised above the limit so that Derive does; a time
	// limit still wins over it). A concurrent case of the same job that was running at that moment is taken
	// for the same reason: the job as a whole went over.
	res, err, oom := j.oomEnded(ctx, res, err, oomBefore)
	if err != nil {
		return sandbox.Result{}, runnerFailure(ctx, "run case "+name, err)
	}
	if oom {
		res.MemoryBytes = max(res.MemoryBytes, r.MaxMemory+1)
	}
	return res, nil
}
