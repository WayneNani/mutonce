package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WayneNani/mutonce/internal/mutant"
)

// edgeSrc collects the cases the MC/DC probes must not change: the tests
// fail if a probe alters a result, and the run reports that as a baseline
// failure.
const edgeSrc = `package edge

import "sync"

type Flag bool

const verbose = false

var names = []string{"a"}

// Ready is a decision at package level, evaluated during initialisation.
var Ready = len(names) > 0 && names[0] != ""

func id(x bool) bool { return x }

// Nested holds a decision inside a condition of another one.
func Nested(a, b, c bool) bool { return id(a && b) || c }

func Not(a, b bool) bool { return !(a && b) }

// WithConst has a constant operand, which is no condition.
func WithConst(a, b bool) bool { return verbose || a && b }

// Recovered must call recover directly in the deferred function, so its
// decision stays uninstrumented.
func Recovered(a bool) (ok bool) {
	defer func() {
		if recover() != nil && a {
			ok = true
		}
	}()
	panic("boom")
}

// Named yields a named bool type, which a probe closure cannot return.
func Named(a, b bool) Flag { return Flag(a) && Flag(b) }

func Generic[T comparable](x, y, z T) bool { return x == y && y != z }

// Depth evaluates its first decision again inside one of its conditions;
// each evaluation needs a vector of its own.
func Depth(n int, a bool) bool {
	if n > 0 && Depth(n-1, a) {
		return true
	}
	return a && n == 0
}

// Parallel evaluates the same decision in several goroutines at once.
func Parallel(xs []int) int {
	var wg sync.WaitGroup
	var mu sync.Mutex
	count := 0
	for _, x := range xs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if x > 0 && x%2 == 0 {
				mu.Lock()
				count++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return count
}
`

const edgeTest = `package edge

import "testing"

func TestEdge(t *testing.T) {
	if !Ready {
		t.Error("Ready")
	}
	if !Nested(true, true, false) || Nested(false, true, false) || !Nested(false, false, true) {
		t.Error("Nested")
	}
	if Not(true, true) || !Not(false, true) {
		t.Error("Not")
	}
	if !WithConst(true, true) || WithConst(true, false) {
		t.Error("WithConst")
	}
	if !Recovered(true) || Recovered(false) {
		t.Error("Recovered")
	}
	if !Named(true, true) {
		t.Error("Named")
	}
	if !Generic(1, 1, 2) || Generic("a", "b", "c") {
		t.Error("Generic")
	}
	if !Depth(1, true) {
		t.Error("Depth")
	}
	if got := Parallel([]int{1, 2, 3, 4, 6}); got != 3 {
		t.Errorf("Parallel = %d", got)
	}
}
`

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestMCDCEdgeCases(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module")
	}
	// Many has more conditions than a vector holds.
	var many []string
	for i := range 33 {
		many = append(many, fmt.Sprintf("x == %d", i))
	}
	dir := writeModule(t, map[string]string{
		"go.mod":       "module edge\n\ngo 1.24\n",
		"edge.go":      edgeSrc,
		"many.go":      "package edge\n\nfunc Many(x int) bool { return " + strings.Join(many, " || ") + " }\n",
		"edge_test.go": edgeTest,
	})
	for _, opsArg := range []string{"none", "all"} {
		t.Run("ops="+opsArg, func(t *testing.T) {
			ops, _ := mutant.ParseOps(opsArg)
			rep, err := Run(Config{Dir: dir, Patterns: []string{"./..."}, Ops: ops, Mode: "schema", Workers: 2, MCDC: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.BaselineFailures) > 0 {
				t.Fatalf("the probes change behaviour: %v", rep.BaselineFailures)
			}
			// Decisions that cannot be measured are reported with the reason.
			reasons := map[string]string{}
			for _, d := range rep.MCDCSkipped {
				reasons[d.Func] = d.Reason
			}
			want := map[string]string{
				"Recovered.func": mutant.SkipRecover,
				"Named":          mutant.SkipNamedBool,
				"Many":           mutant.SkipManyConds,
			}
			if fmt.Sprint(reasons) != fmt.Sprint(want) {
				t.Errorf("skipped decisions: %v, want %v", reasons, want)
			}
			total := rep.MCDCTotal
			if total == nil || total.NotMeasured != 3 || total.Decisions != len(rep.MCDC)+3 {
				t.Fatalf("summary: %+v with %d measured decisions", total, len(rep.MCDC))
			}
			// The 2 + 2 + 33 conditions of the skipped decisions count as not covered.
			measured := 0
			for _, d := range rep.MCDC {
				measured += len(d.Conditions)
			}
			if total.Conditions != measured+37 || total.Percent >= 100 {
				t.Errorf("summary: %+v, %d measured conditions", total, measured)
			}
			if !rep.MCDCBelow(100) || rep.MCDCBelow(total.Percent) {
				t.Errorf("MCDCBelow is off for %.2f %%", total.Percent)
			}
			if len(rep.MCDCPackages) != 1 || rep.MCDCPackages[0].Pkg != "edge" || rep.MCDCPackages[0].Covered != total.Covered {
				t.Errorf("package summary: %+v", rep.MCDCPackages)
			}
			var out strings.Builder
			rep.Print(&out, false)
			if !strings.Contains(out.String(), "left out: "+mutant.SkipNamedBool) || !strings.Contains(out.String(), "3 not measured") {
				t.Errorf("report lacks the skipped decisions:\n%s", out.String())
			}
			data, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"reason":"calls recover"`) || !strings.Contains(string(data), `"mcdcTotal":{`) {
				t.Errorf("JSON lacks reasons or summary")
			}
			for _, o := range rep.Outcomes {
				if o.Status == Error {
					t.Errorf("mutant #%d: %s", o.ID, o.Detail)
				}
			}
			byExpr := map[string]mutant.DecisionResult{}
			for _, d := range rep.MCDC {
				byExpr[d.Expr] = d
			}
			for _, expr := range []string{
				`len(names) > 0 && names[0] != ""`, "id(a && b) || c", "a && b", "!(a && b)",
				"verbose || a && b", "x == y && y != z", "n > 0 && Depth(n-1, a)", "a && n == 0", "x > 0 && x%2 == 0",
			} {
				if _, ok := byExpr[expr]; !ok {
					t.Errorf("no decision %q", expr)
				}
			}
			for expr := range byExpr {
				if strings.Contains(expr, "recover()") || strings.Contains(expr, "Flag(") {
					t.Errorf("decision %q must not be instrumented", expr)
				}
			}
			if d := byExpr["verbose || a && b"]; len(d.Conditions) != 2 {
				t.Errorf("constant counted as a condition: %+v", d.Conditions)
			}
			// Depth(1) evaluates n > 0 && … as (true, true) → true around an
			// inner (false, –) → false; a shared vector would mix them.
			if d := byExpr["n > 0 && Depth(n-1, a)"]; d.Vectors != 2 || !d.Conditions[0].Covered || d.Conditions[1].Covered {
				t.Errorf("recursive decision: %d vectors, %+v", d.Vectors, d.Conditions)
			}
			if d := byExpr["x > 0 && x%2 == 0"]; d.Covered != 1 || !d.Conditions[1].Covered {
				t.Errorf("parallel decision: %+v", d.Conditions)
			}
			if d := byExpr["!(a && b)"]; d.Covered != 1 || d.Conditions[1].Suggestion != "a = true, b = false → true" {
				t.Errorf("negated decision: %+v", d.Conditions)
			}
		})
	}
}

// MC/DC must not change the verdict of any mutant.
func TestMCDCKeepsMutationResults(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the example module twice")
	}
	dir, err := filepath.Abs("../../example")
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := mutant.ParseOps("all")
	cfg := Config{Dir: dir, Patterns: []string{"./..."}, Ops: ops, Mode: "schema", Workers: 4, TimeoutFactor: 10}
	plain, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MCDC = true
	withMCDC, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Outcomes) != len(withMCDC.Outcomes) {
		t.Fatalf("%d mutants without MC/DC, %d with", len(plain.Outcomes), len(withMCDC.Outcomes))
	}
	for i, o := range plain.Outcomes {
		if m := withMCDC.Outcomes[i]; m.ID != o.ID || m.Status != o.Status {
			t.Errorf("mutant #%d %s → %s: %s without MC/DC, %s with (by %s, %s)", o.ID, o.Original, o.Mutated, o.Status, m.Status, m.KilledBy, m.Detail)
		}
	}
	var free *mutant.DecisionResult
	for i, d := range withMCDC.MCDC {
		if d.Func == "FreeShipping" {
			free = &withMCDC.MCDC[i]
		}
	}
	if free == nil {
		t.Fatal("no decision in FreeShipping")
	}
	// The tests never charge shipping: premium = false, total <= 2500.
	if free.Covered != 0 || free.Conditions[0].Suggestion != "premium = false, total > 2500 = false → false" {
		t.Errorf("FreeShipping: %+v", free.Conditions)
	}
	var out strings.Builder
	withMCDC.Print(&out, false)
	if !strings.Contains(out.String(), "missing: premium = false, total > 2500 = false → false") {
		t.Errorf("report lacks the missing test case:\n%s", out.String())
	}
}
