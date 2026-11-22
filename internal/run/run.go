// Package run builds the schema test binaries once, records which test
// reaches which mutation point, and executes every mutant as a separate
// process of an already built binary.
package run

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/WayneNani/mutonce/internal/mutant"
)

// Config controls a run.
type Config struct {
	Dir           string // module directory
	Patterns      []string
	Ops           map[mutant.Op]bool
	Mode          string // "schema" or "naive"
	Workers       int
	TimeoutFactor float64
	MemFactor     float64  // memory limit = factor × peak RSS without mutation
	MemBudget     int64    // bytes shared by all test processes; 0 = half the available RAM
	Limit         int      // execute only an even sample of this many covered mutants
	Diff          string   // only mutate lines changed since this git ref
	TestPatterns  []string // packages whose tests run; default: the mutated ones
	Recheck       bool     // run every mutant twice and flag changing verdicts
	NoSandbox     bool     // run tests in the module itself instead of copies
	Reorder       bool     // run the last killer of a function first; no gain measured so far
	Keep          bool
	Log           io.Writer
}

// Status is the outcome of a mutant.
type Status string

const (
	Killed     Status = "KILLED"
	Timeout    Status = "TIMEOUT"
	Memory     Status = "MEMORY" // exceeded its memory limit
	Survived   Status = "SURVIVED"
	NotCovered Status = "NOT_COVERED"
	Flaky      Status = "FLAKY" // verdict changed when run again
	Error      Status = "ERROR"
)

// Outcome is the result for one mutant.
type Outcome struct {
	mutant.Mutant
	Status   Status        `json:"status"`
	Tests    int           `json:"tests"`
	KilledBy string        `json:"killedBy,omitempty"`
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"durationNs"`
}

// Phase is a timed step of the run.
type Phase struct {
	Name     string        `json:"name"`
	Duration time.Duration `json:"durationNs"`
}

// Report summarises a run.
type Report struct {
	Mode             string          `json:"mode"`
	Dir              string          `json:"dir"`
	Outcomes         []Outcome       `json:"outcomes"`
	Excluded         []mutant.Mutant `json:"excluded"`
	Rounds           int             `json:"rounds"`
	Binaries         int             `json:"binaries"`
	Tests            int             `json:"tests"`
	Covered          int             `json:"covered"`
	Executed         int             `json:"executed"`
	Phases           []Phase         `json:"phases"`
	BaselineFailures []string        `json:"baselineFailures,omitempty"`
	MemBudget        int64           `json:"memBudget"`
	Diff             string          `json:"diff,omitempty"`
	DiffFiles        int             `json:"diffFiles,omitempty"`
	DiffLines        int             `json:"diffLines,omitempty"`
	Unselected       int             `json:"unselected,omitempty"`
	SideEffects      []string        `json:"sideEffects,omitempty"` // files test runs left behind (removed again)
	WorkDir          string          `json:"workDir,omitempty"`
}

type binary struct {
	idx  int
	pkg  mutant.PkgInfo
	path string
}

type testRef struct {
	bin  *binary
	name string
	dur  time.Duration
	mem  int64 // peak RSS without mutation
}

type runner struct {
	cfg     Config
	res     *mutant.Result
	work    string
	replace map[string]string // overlay: original path -> generated file

	// Every test process reserves its memory limit from this budget before
	// it starts, so all of them together cannot exhaust the machine. A
	// mutant that allocates without bound would otherwise take the OOM
	// killer, and with it whatever shares its cgroup.
	mem    *semaphore.Weighted
	budget int64

	killersMu sync.Mutex
	killers   map[string]testKey // file + function -> test that last killed a mutant there

	boxes *sandboxes // nil: tests run in the module itself
}

const minMemLimit = 512 << 20

// defaultMemBudget applies where the free memory cannot be determined.
const defaultMemBudget = 4 << 30

// Run executes a complete mutation analysis.
func Run(cfg Config) (*Report, error) {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.MemFactor <= 0 {
		cfg.MemFactor = 4
	}
	if cfg.TimeoutFactor <= 0 {
		cfg.TimeoutFactor = 5
	}
	budget := cfg.MemBudget
	if budget <= 0 {
		if avail, err := memAvailable(); err == nil {
			budget = avail / 2
		} else {
			budget = defaultMemBudget
			fmt.Fprintf(cfg.Log, "free memory unknown (%v), using a budget of %d MiB; set one with -mem-budget\n", err, budget>>20)
		}
	}
	if budget < minMemLimit {
		return nil, fmt.Errorf("memory budget of %d MiB is below the minimum of %d MiB per process", budget>>20, minMemLimit>>20)
	}
	start := time.Now()
	rep := &Report{Mode: cfg.Mode, Dir: cfg.Dir, MemBudget: budget}
	phase := func(name string, since time.Time) {
		rep.Phases = append(rep.Phases, Phase{name, time.Since(since)})
	}
	logf := func(format string, args ...any) { fmt.Fprintf(cfg.Log, format+"\n", args...) }

	t := time.Now()
	var inDiff func(mutant.Mutant) bool
	if cfg.Diff != "" {
		changes, err := mutant.ChangedLines(cfg.Dir, cfg.Diff)
		if err != nil {
			return nil, err
		}
		rep.Diff, rep.DiffFiles, rep.DiffLines = cfg.Diff, len(changes), changes.Lines()
		inDiff = changes.Touches
	}
	res, err := mutant.Generate(cfg.Dir, cfg.Patterns, cfg.Ops, inDiff, logf)
	if err != nil {
		return nil, err
	}
	phase("Schema + type check", t)
	rep.Excluded, rep.Rounds, rep.Unselected = res.Excluded, res.Rounds, res.Unselected
	logf("%d mutants in the schema, %d dropped", len(res.Mutants), len(res.Excluded))
	if len(res.Mutants) == 0 {
		rep.Phases = append(rep.Phases, Phase{"Total", time.Since(start)})
		return rep, nil
	}

	work, err := os.MkdirTemp("", "mutonce-")
	if err != nil {
		return nil, err
	}
	if cfg.Keep {
		rep.WorkDir = work
	} else {
		defer os.RemoveAll(work)
	}
	r := &runner{
		cfg:     cfg,
		res:     res,
		work:    work,
		replace: map[string]string{},
		mem:     semaphore.NewWeighted(budget),
		budget:  budget,
		killers: map[string]testKey{},
	}
	if !cfg.NoSandbox {
		root, err := moduleRoot(cfg.Dir)
		if err != nil {
			return nil, err
		}
		if r.boxes, err = newSandboxes(root, work, cfg.Workers); err != nil {
			return nil, fmt.Errorf("creating sandboxes: %w", err)
		}
		defer func() { rep.SideEffects = r.boxes.sideEffects() }()
	}

	t = time.Now()
	overlay, err := r.writeOverlay()
	if err != nil {
		return nil, err
	}
	testPkgs := res.Pkgs
	if len(cfg.TestPatterns) > 0 {
		if testPkgs, err = listPackages(cfg.Dir, cfg.TestPatterns); err != nil {
			return nil, err
		}
	}
	bins, err := r.buildAll(overlay, testPkgs)
	if err != nil {
		return nil, err
	}
	phase("Build test binaries", t)
	rep.Binaries = len(bins)
	logf("%d test binaries built", len(bins))

	t = time.Now()
	cov, tests, failures, err := r.record(bins)
	if err != nil {
		return nil, err
	}
	phase("Record per-test coverage", t)
	rep.Tests, rep.BaselineFailures = tests, failures

	var covered []mutant.Mutant
	for _, m := range res.Mutants {
		if len(cov[m.ID]) == 0 {
			rep.Outcomes = append(rep.Outcomes, Outcome{Mutant: m, Status: NotCovered})
		} else {
			covered = append(covered, m)
		}
	}
	rep.Covered = len(covered)
	selected := sample(covered, cfg.Limit)
	rep.Executed = len(selected)
	logf("%d mutants covered, running %d (%s)", len(covered), len(selected), cfg.Mode)

	t = time.Now()
	outcomes := make([]Outcome, len(selected))
	var g errgroup.Group
	g.SetLimit(cfg.Workers)
	for i, m := range selected {
		g.Go(func() error {
			outcomes[i] = r.exec(m, cov[m.ID])
			return nil
		})
	}
	g.Wait()
	phase("Run mutants", t)
	rep.Outcomes = append(rep.Outcomes, outcomes...)
	sort.Slice(rep.Outcomes, func(i, j int) bool { return rep.Outcomes[i].ID < rep.Outcomes[j].ID })
	rep.Phases = append(rep.Phases, Phase{"Total", time.Since(start)})
	return rep, nil
}

func (r *runner) writeOverlay() (string, error) {
	dir := filepath.Join(r.work, "overlay")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	paths := make([]string, 0, len(r.res.Overlay))
	for p := range r.res.Overlay {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for i, p := range paths {
		dst := filepath.Join(dir, fmt.Sprintf("%03d-%s", i, filepath.Base(p)))
		if err := os.WriteFile(dst, r.res.Overlay[p], 0o644); err != nil {
			return "", err
		}
		r.replace[p] = dst
	}
	return writeOverlayJSON(filepath.Join(r.work, "overlay.json"), r.replace)
}

func writeOverlayJSON(path string, replace map[string]string) (string, error) {
	data, err := json.MarshalIndent(map[string]any{"Replace": replace}, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o644)
}

// buildAll builds one test binary per package that has tests. Each binary
// contains the schema of every mutated package it depends on.
func (r *runner) buildAll(overlay string, pkgs []mutant.PkgInfo) ([]*binary, error) {
	var bins []*binary
	for _, p := range pkgs {
		if p.HasTests {
			bins = append(bins, &binary{idx: len(bins), pkg: p})
		}
	}
	if err := os.MkdirAll(filepath.Join(r.work, "bin"), 0o755); err != nil {
		return nil, err
	}
	var g errgroup.Group
	g.SetLimit(max(1, r.cfg.Workers/2))
	for _, b := range bins {
		g.Go(func() error {
			out := filepath.Join(r.work, "bin", fmt.Sprintf("%03d.test%s", b.idx, exeSuffix))
			if err := r.goTestC(overlay, out, b.pkg.Path); err != nil {
				return err
			}
			if _, err := os.Stat(out); err == nil {
				b.path = out
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	built := bins[:0]
	for _, b := range bins {
		if b.path != "" {
			built = append(built, b)
		}
	}
	return built, nil
}

func (r *runner) goTestC(overlay, out, pkgPath string) error {
	cmd := exec.Command("go", "test", "-c", "-vet=off", "-overlay="+overlay, "-o", out, pkgPath)
	cmd.Dir = r.cfg.Dir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go test -c %s: %v\n%s", pkgPath, err, output)
	}
	return nil
}

// record runs every test once on its own with recording enabled. That costs
// one run per test, not per test and mutant, and yields the per-test
// coverage that `go test -cover` cannot provide.
func (r *runner) record(bins []*binary) (map[int][]testRef, int, []string, error) {
	type job struct {
		bin  *binary
		name string
		file string
	}
	var jobs []job
	for _, b := range bins {
		names, err := listTests(b)
		if err != nil {
			return nil, 0, nil, err
		}
		for _, n := range names {
			jobs = append(jobs, job{b, n, filepath.Join(r.work, "rec", fmt.Sprintf("%03d-%05d", b.idx, len(jobs)))})
		}
	}
	if err := os.MkdirAll(filepath.Join(r.work, "rec"), 0o755); err != nil {
		return nil, 0, nil, err
	}

	cov := map[int][]testRef{}
	var failures []string
	var mu sync.Mutex
	var g errgroup.Group
	g.SetLimit(r.cfg.Workers)
	limit := min(max(1<<30, r.budget/int64(r.cfg.Workers)), r.budget)
	for _, j := range jobs {
		g.Go(func() error {
			run := r.runLimited(j.bin, []string{j.name}, []string{"MUTONCE_RECORD=" + j.file}, 10*time.Minute, limit)
			ids, err := readIDs(j.file)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				return err
			}
			if !run.passed {
				failures = append(failures, j.bin.pkg.Path+"."+j.name)
				return nil // a failing test cannot kill anything
			}
			for _, id := range ids {
				cov[id] = append(cov[id], testRef{j.bin, j.name, run.dur, run.maxRSS})
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, 0, nil, err
	}
	for _, refs := range cov {
		sort.Slice(refs, func(i, k int) bool {
			if refs[i].bin.idx != refs[k].bin.idx {
				return refs[i].bin.idx < refs[k].bin.idx
			}
			return refs[i].name < refs[k].name
		})
	}
	sort.Strings(failures)
	return cov, len(jobs), failures, nil
}

var testName = regexp.MustCompile(`^(Test|Example|Fuzz)\w*$`)

func listTests(b *binary) ([]string, error) {
	cmd := exec.Command(b.path, "-test.list", ".")
	cmd.Dir = b.pkg.Dir
	cmd.Env = cleanEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing tests of %s: %v", b.pkg.Path, err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); testName.MatchString(line) {
			names = append(names, line)
		}
	}
	return names, nil
}

func readIDs(path string) ([]int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ids []int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id, err := strconv.Atoi(sc.Text()); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, sc.Err()
}

// sample picks n evenly spaced mutants so that schema and naive mode run the
// same subset.
func sample(ms []mutant.Mutant, n int) []mutant.Mutant {
	if n <= 0 || n >= len(ms) {
		return ms
	}
	out := make([]mutant.Mutant, n)
	for i := range n {
		out[i] = ms[i*len(ms)/n]
	}
	return out
}

type group struct {
	bin   *binary
	names []string
	dur   time.Duration
	mem   int64 // largest peak RSS of the tests
}

func groupByBinary(refs []testRef) []group {
	var gs []group
	for _, ref := range refs {
		if len(gs) == 0 || gs[len(gs)-1].bin != ref.bin {
			gs = append(gs, group{bin: ref.bin})
		}
		g := &gs[len(gs)-1]
		g.names = append(g.names, ref.name)
		g.dur += ref.dur
		g.mem = max(g.mem, ref.mem)
	}
	return gs
}

func (r *runner) timeout(g group) time.Duration {
	return time.Duration(r.cfg.TimeoutFactor*float64(g.dur)) + 2*time.Second
}

// memLimit is the memory counterpart of the timeout.
func (r *runner) memLimit(g group) int64 {
	return min(max(minMemLimit, int64(r.cfg.MemFactor*float64(g.mem))), r.budget)
}

// runLimited reserves the memory limit from the shared budget for the
// duration of the run.
func (r *runner) runLimited(b *binary, names, env []string, timeout time.Duration, limit int64) testRun {
	if err := r.mem.Acquire(context.Background(), limit); err != nil {
		return testRun{err: err}
	}
	defer r.mem.Release(limit)
	if r.boxes == nil {
		return runTests(b, b.pkg.Dir, names, env, timeout, limit)
	}
	return r.boxes.run(b.pkg.Dir, func(dir string) testRun {
		return runTests(b, dir, names, env, timeout, limit)
	})
}

// exec runs a mutant and, with Recheck, a second time: a mutant whose
// verdict changes depends on chance, not on the tests' strength.
func (r *runner) exec(m mutant.Mutant, refs []testRef) Outcome {
	run := r.execSchema
	if r.cfg.Mode == "naive" {
		run = r.execNaive
	}
	first := run(m, refs)
	if !r.cfg.Recheck || first.Status == Error {
		return first
	}
	second := run(m, refs)
	detected := func(s Status) bool { return s == Killed || s == Timeout || s == Memory }
	if second.Status != Error && detected(first.Status) != detected(second.Status) {
		first.Detail = fmt.Sprintf("1st run: %s, 2nd run: %s", first.Status, second.Status)
		first.Status = Flaky
	}
	first.Duration += second.Duration
	return first
}

// execSchema runs only the tests that reach the mutant, in the binaries
// built once, with the mutant switched on by an environment variable.
func (r *runner) execSchema(m mutant.Mutant, refs []testRef) Outcome {
	start := time.Now()
	out := Outcome{Mutant: m, Tests: len(refs)}
	env := []string{"MUTONCE_ACTIVE=" + strconv.Itoa(m.ID)}
	for _, g := range r.order(m, refs) {
		run := r.runLimited(g.bin, g.names, env, r.timeout(g), r.memLimit(g))
		if classify(&out, g, run) {
			if out.Status == Killed {
				r.rememberKiller(m, g, run.output)
			}
			break
		}
	}
	if out.Status == "" {
		out.Status = Survived
	}
	out.Duration = time.Since(start)
	return out
}

type testKey struct {
	bin  int
	name string
}

// order puts the test that last killed a mutant of the same function in a
// run of its own before the others. Neighbouring mutants tend to fall to the
// same test, and a go test binary runs tests in source order no matter how
// -test.run lists them, so only a separate run can move it to the front.
func (r *runner) order(m mutant.Mutant, refs []testRef) []group {
	r.killersMu.Lock()
	k, ok := r.killers[m.File+"\x00"+m.Func]
	r.killersMu.Unlock()
	if ok && len(refs) > 1 && r.cfg.Reorder {
		for i, ref := range refs {
			if ref.bin.idx == k.bin && ref.name == k.name {
				rest := append(slices.Clone(refs[:i]), refs[i+1:]...)
				first := group{bin: ref.bin, names: []string{ref.name}, dur: ref.dur, mem: ref.mem}
				return append([]group{first}, groupByBinary(rest)...)
			}
		}
	}
	return groupByBinary(refs)
}

func (r *runner) rememberKiller(m mutant.Mutant, g group, output []byte) {
	name, _, _ := strings.Cut(killer(output), "/") // top-level test of a subtest
	if !slices.Contains(g.names, name) {
		return
	}
	r.killersMu.Lock()
	r.killers[m.File+"\x00"+m.Func] = testKey{g.bin.idx, name}
	r.killersMu.Unlock()
}

// execNaive is the conventional approach for comparison: a separate build
// per mutant. The mutant is hard-coded as a constant, so the compiler drops
// all other variants; test selection is the same as in schema mode.
func (r *runner) execNaive(m mutant.Mutant, refs []testRef) Outcome {
	start := time.Now()
	out := Outcome{Mutant: m, Tests: len(refs)}
	fail := func(err error) Outcome {
		out.Status, out.Detail = Error, err.Error()
		out.Duration = time.Since(start)
		return out
	}

	dir := filepath.Join(r.work, "naive", strconv.Itoa(m.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	host := r.res.Runtime[m.Pkg]
	hostFile := filepath.Join(dir, filepath.Base(host))
	if err := os.WriteFile(hostFile, mutant.WithActive(r.res.Overlay[host], m.ID), 0o644); err != nil {
		return fail(err)
	}
	replace := maps.Clone(r.replace)
	replace[host] = hostFile
	overlay, err := writeOverlayJSON(filepath.Join(dir, "overlay.json"), replace)
	if err != nil {
		return fail(err)
	}

	for _, g := range groupByBinary(refs) {
		bin := &binary{idx: g.bin.idx, pkg: g.bin.pkg, path: filepath.Join(dir, fmt.Sprintf("%03d.test%s", g.bin.idx, exeSuffix))}
		if err := r.goTestC(overlay, bin.path, bin.pkg.Path); err != nil {
			return fail(err)
		}
		g.bin = bin
		if classify(&out, g, r.runLimited(bin, g.names, nil, r.timeout(g), r.memLimit(g))) {
			break
		}
	}
	if out.Status == "" {
		out.Status = Survived
	}
	out.Duration = time.Since(start)
	return out
}

// classify records a decisive test run and reports whether it was one.
func classify(out *Outcome, g group, run testRun) bool {
	switch {
	case run.err != nil:
		out.Status, out.Detail = Error, run.err.Error()
	case run.timedOut:
		out.Status, out.KilledBy = Timeout, g.bin.pkg.Path
	case run.oom:
		out.Status, out.KilledBy = Memory, g.bin.pkg.Path
	case !run.passed:
		out.Status, out.KilledBy = Killed, g.bin.pkg.Path+"."+killer(run.output)
	default:
		return false
	}
	return true
}

var failLine = regexp.MustCompile(`--- FAIL: (\S+)`)

func killer(output []byte) string {
	if m := failLine.FindSubmatch(output); m != nil {
		return string(m[1])
	}
	for _, s := range []string{"fatal error", "panic"} {
		if bytes.Contains(output, []byte(s+":")) {
			return "(" + s + ")"
		}
	}
	return "(exit)"
}

type testRun struct {
	passed   bool
	timedOut bool
	oom      bool
	dur      time.Duration
	maxRSS   int64
	output   []byte
	err      error
}

// runTests runs the named top-level tests of a binary under a procGuard, so
// mutants that loop, deadlock or allocate without bound are stopped together
// with any child processes.
func runTests(b *binary, dir string, names []string, env []string, timeout time.Duration, memLimit int64) testRun {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	cmd := exec.CommandContext(ctx, b.path, "-test.run", "^("+strings.Join(quoted, "|")+")$", "-test.failfast")
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), env...)
	guard := newGuard(cmd, memLimit)
	cmd.WaitDelay = time.Second
	out := &capped{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, out

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return testRun{err: err}
	}
	if err := guard.started(cmd); err != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		guard.finished(cmd)
		return testRun{err: err}
	}
	err := cmd.Wait()
	run := testRun{dur: time.Since(start), output: out.bytes()}
	var exceeded bool
	run.maxRSS, exceeded = guard.finished(cmd)
	var exit *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		run.timedOut = true
	case exceeded:
		run.oom = true
	case err == nil:
		run.passed = true
	case !errors.As(err, &exit):
		run.err = err
	case outOfMemory.Match(run.output):
		run.oom = true
	}
	return run
}

// outOfMemory matches the runtime's messages, e.g. "fatal error: runtime: out
// of memory" or "fatal error: out of memory allocating heap arena metadata".
var outOfMemory = regexp.MustCompile(`fatal error: [^\n]*out of memory|cannot allocate memory`)

// listPackages lists the packages matched by patterns, for choosing the
// tests independently of the mutated packages.
func listPackages(dir string, patterns []string) ([]mutant.PkgInfo, error) {
	args := append([]string{"list", "-f", "{{.ImportPath}}\t{{.Name}}\t{{.Dir}}\t{{len .TestGoFiles}}\t{{len .XTestGoFiles}}"}, patterns...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %v: %s", strings.Join(patterns, " "), err, strings.TrimSpace(stderr.String()))
	}
	var pkgs []mutant.PkgInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			continue
		}
		pkgs = append(pkgs, mutant.PkgInfo{Path: f[0], Name: f[1], Dir: f[2], HasTests: f[3] != "0" || f[4] != "0"})
	}
	return pkgs, nil
}

// moduleRoot is the directory of the go.mod that governs dir.
func moduleRoot(dir string) (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("%s is not inside a Go module", dir)
	}
	return filepath.Dir(gomod), nil
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MUTONCE_") {
			env = append(env, kv)
		}
	}
	return env
}

// capped keeps the first bytes of the output, since a mutant may print
// forever, plus the last few, where the runtime reports fatal errors.
type capped struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	tail  []byte
	limit int
}

const tailSize = 4 << 10

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.limit - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	c.tail = append(c.tail, p[max(0, len(p)-tailSize):]...)
	if len(c.tail) > tailSize {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-tailSize:]...)
	}
	return len(p), nil
}

func (c *capped) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buf.Len() < c.limit {
		return c.buf.Bytes()
	}
	return append(append(c.buf.Bytes(), "\n…\n"...), c.tail...)
}
