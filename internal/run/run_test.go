package run

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/WayneNani/mutonce/internal/mutant"
)

func runExample(t *testing.T, mode string, limit int) *Report {
	t.Helper()
	dir, err := filepath.Abs("../../example")
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := mutant.ParseOps("all")
	rep, err := Run(Config{
		Dir:           dir,
		Patterns:      []string{"./..."},
		Ops:           ops,
		Mode:          mode,
		Workers:       runtime.NumCPU(),
		TimeoutFactor: 10,
		Limit:         limit,
	})
	if err != nil {
		t.Fatalf("Run(%s): %v", mode, err)
	}
	if len(rep.BaselineFailures) > 0 {
		t.Fatalf("tests fail without mutation: %v", rep.BaselineFailures)
	}
	for _, o := range rep.Outcomes {
		if o.Status == Error {
			t.Errorf("mutant #%d: %s", o.ID, o.Detail)
		}
	}
	return rep
}

func find(t *testing.T, rep *Report, fn, orig, mutated string) Outcome {
	t.Helper()
	for _, o := range rep.Outcomes {
		if o.Func == fn && o.Original == orig && o.Mutated == mutated {
			return o
		}
	}
	t.Fatalf("no mutant %q → %q in %s", orig, mutated, fn)
	return Outcome{}
}

func TestExample(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the example module")
	}
	rep := runExample(t, "schema", 0)

	for _, tc := range []struct {
		fn, orig, mutated string
		want              Status
	}{
		// The tests never probe the discount boundary.
		{"Discount", "total >= 10000", "total > 10000", Survived},
		// Removing the halving turns the loop into an endless one.
		{"Batches", "n /= 2", "statement removed", Timeout},
		// Appending forever must hit the memory limit, not the machine
		// (or, where only resident memory is watched, possibly the timeout).
		{"Repeat", "i <= n", "true", Memory},
		// Go-specific: a swallowed error and a select that blocks.
		{"(*Store).Take", "return ErrInsufficient", "return nil", Killed},
		{"TryNotify", "select { … default: … }", "default clause removed", Killed},
		// mathx has no tests; pricing's tests reach it.
		{"Clamp", "func Clamp", "returns zero values", Killed},
		// Short-circuit: the original must not dereference nil.
		{"Positive", "n != nil && n.Val > 0", "n.Val > 0", Killed},
		// Never called by any test.
		{"Max", "func Max", "returns zero values", NotCovered},
	} {
		got := find(t, rep, tc.fn, tc.orig, tc.mutated)
		if got.Status == Timeout && tc.want == Memory && runtime.GOOS != "linux" {
			// Only Linux has a hard, deterministic limit. Elsewhere memory
			// that compresses well (macOS) or slow runners can let the
			// timeout win the race; both verdicts mean the mutant was stopped.
			continue
		}
		if got.Status != tc.want {
			t.Errorf("%s: %s → %s: %s, want %s", tc.fn, tc.orig, tc.mutated, got.Status, tc.want)
		}
	}
	if len(rep.Excluded) == 0 {
		t.Error("expected the type-check filter to drop the returns in Sign")
	}

	// notePath mutated to "" writes ".tmp" into the working directory: in
	// the sandbox, reported, and not in the example itself.
	if !slices.Contains(rep.SideEffects, filepath.Join("tricky", ".tmp")) {
		t.Errorf("side effect not reported: %v", rep.SideEffects)
	}
	if _, err := os.Stat("../../example/tricky/.tmp"); err == nil {
		t.Error("mutant left tricky/.tmp in the example")
	}
}

// The diff mode only mutates code on changed lines.
func TestDiff(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the example module")
	}
	src, _ := filepath.Abs("../../example")
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "start")

	file := filepath.Join(dir, "pricing", "pricing.go")
	data, _ := os.ReadFile(file)
	changed := strings.Replace(string(data), "case total >= 10000:", "case total >= 10_000:", 1)
	if err := os.WriteFile(file, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}

	ops, _ := mutant.ParseOps("all")
	rep, err := Run(Config{Dir: dir, Patterns: []string{"./..."}, Ops: ops, Mode: "schema",
		Workers: runtime.NumCPU(), TimeoutFactor: 5, Diff: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.DiffFiles != 1 || rep.DiffLines != 1 {
		t.Errorf("diff: %d files, %d lines; want 1, 1", rep.DiffFiles, rep.DiffLines)
	}
	got := map[string]bool{}
	for _, o := range rep.Outcomes {
		if o.File != file {
			t.Errorf("mutant outside the diff: %s:%d %s", o.File, o.Line, o.Original)
		}
		got[o.Original+" → "+o.Mutated] = true
	}
	for _, want := range []string{
		"total >= 10_000 → total > 10_000",    // on the changed line
		"func Discount → returns zero values", // the function containing it
	} {
		if !got[want] {
			t.Errorf("missing mutant %q; got %v", want, got)
		}
	}
	if rep.Unselected == 0 {
		t.Error("no mutants were left out")
	}
}

// mathx has no tests of its own. Mutated alone, all its mutants are
// uncovered; with the tests of the whole module, pricing's tests reach it.
func TestTestPatterns(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the example module")
	}
	dir, _ := filepath.Abs("../../example")
	ops, _ := mutant.ParseOps("RET")
	zaehle := func(rep *Report) (covered int) {
		for _, o := range rep.Outcomes {
			if o.Status != NotCovered {
				covered++
			}
		}
		return
	}
	cfg := Config{Dir: dir, Patterns: []string{"./internal/mathx"}, Ops: ops, Mode: "schema", Workers: runtime.NumCPU()}
	allein, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TestPatterns = []string{"./..."}
	mitAllen, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(allein.Outcomes) == 0 || zaehle(allein) != 0 {
		t.Errorf("mathx alone: %d mutants, %d covered; want all uncovered", len(allein.Outcomes), zaehle(allein))
	}
	if zaehle(mitAllen) == 0 || mitAllen.Binaries < 3 {
		t.Errorf("with -tests ./...: %d covered in %d binaries", zaehle(mitAllen), mitAllen.Binaries)
	}
}

// A test that checks only on every other run makes its mutant flaky.
func TestRecheckFlaky(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module")
	}
	dir := t.TempDir()
	// The counter lives outside the module: the sandbox removes files a run
	// leaves in the package directory, which would reset it every time.
	zaehler := strconv.Quote(filepath.Join(t.TempDir(), "count"))
	files := map[string]string{
		"go.mod":  "module flaky\n\ngo 1.24\n",
		"pick.go": "package flaky\n\nfunc Pick() int { return 1 }\n",
		"pick_test.go": `package flaky

import (
	"os"
	"strconv"
	"testing"
)

// Counts its runs in a file and only checks on odd ones.
func TestPick(t *testing.T) {
	b, _ := os.ReadFile(` + zaehler + `)
	n, _ := strconv.Atoi(string(b))
	n++
	os.WriteFile(` + zaehler + `, []byte(strconv.Itoa(n)), 0o644)
	if n%2 == 1 && Pick() != 1 {
		t.Fatal("wrong pick")
	}
}
`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ops, _ := mutant.ParseOps("RET")
	rep, err := Run(Config{Dir: dir, Patterns: []string{"./..."}, Ops: ops, Mode: "schema", Workers: 1, Recheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Outcomes) != 1 || rep.Outcomes[0].Status != Flaky {
		t.Fatalf("outcomes = %+v, want one FLAKY", rep.Outcomes)
	}
}

// The schema must behave exactly like separately compiled mutants.
func TestSchemaMatchesNaive(t *testing.T) {
	if testing.Short() {
		t.Skip("builds one binary per mutant")
	}
	const sample = 20
	schema := runExample(t, "schema", sample)
	naive := runExample(t, "naive", sample)
	want := map[int]Status{}
	for _, o := range naive.Outcomes {
		want[o.ID] = o.Status
	}
	compared := 0
	for _, o := range schema.Outcomes {
		if o.Status == NotCovered {
			continue
		}
		compared++
		if want[o.ID] != o.Status {
			t.Errorf("mutant #%d %s → %s: schema %s, naive %s", o.ID, o.Original, o.Mutated, o.Status, want[o.ID])
		}
	}
	if compared != sample {
		t.Errorf("compared %d mutants, want %d", compared, sample)
	}
}
