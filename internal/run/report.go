package run

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/WayneNani/mutonce/internal/mutant"
)

// Up to this many survivors are listed one by one; above it the report
// summarises them per function unless -v is given.
const detailLimit = 40

// Print writes a human-readable report.
func (rep *Report) Print(w io.Writer, verbose bool) {
	fmt.Fprintf(w, "\nmutonce – mode: %s\n\n", rep.Mode)
	if rep.Diff != "" {
		fmt.Fprintf(w, "Diff:      %d changed line(s) in %d file(s) since %s, %d mutants outside skipped\n",
			rep.DiffLines, rep.DiffFiles, rep.Diff, rep.Unselected)
	}
	fmt.Fprintf(w, "Mutants:   %d in the schema, %d dropped by the type check (%d round(s))\n",
		len(rep.Outcomes)+rep.Covered-rep.Executed, len(rep.Excluded), rep.Rounds)
	if len(rep.Outcomes) == 0 {
		fmt.Fprintln(w, "\nNo mutants to check.")
		return
	}
	fmt.Fprintf(w, "Tests:     %d in %d test binaries\n", rep.Tests, rep.Binaries)
	fmt.Fprintf(w, "Memory:    budget of %d MiB for all test processes together\n", rep.MemBudget>>20)
	if rep.Executed < rep.Covered {
		fmt.Fprintf(w, "Sample:    ran %d of %d covered mutants\n", rep.Executed, rep.Covered)
	}
	for _, f := range rep.BaselineFailures {
		fmt.Fprintf(w, "WARNING:   test fails even without a mutant: %s\n", f)
	}
	if n := len(rep.SideEffects); n > 0 {
		shown := rep.SideEffects[:min(n, 5)]
		fmt.Fprintf(w, "Side effects: test runs created %d file(s), removed again in the sandbox: %s",
			n, strings.Join(shown, ", "))
		if n > len(shown) {
			fmt.Fprint(w, ", …")
		}
		fmt.Fprintln(w)
	}

	rep.printTable(w)
	rep.printSurvivors(w, verbose)
	if verbose {
		rep.list(w, "Uncovered mutants", NotCovered)
		rep.list(w, "Timeouts", Timeout)
		rep.list(w, "Memory limit exceeded", Memory)
	}
	rep.list(w, "Flaky: verdict changes when run again", Flaky)
	rep.list(w, "Errors", Error)

	fmt.Fprintln(w, "\nTimings:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range rep.Phases {
		extra := ""
		if p.Name == "Run mutants" && rep.Executed > 0 {
			extra = fmt.Sprintf("(%s per mutant)", round(p.Duration/time.Duration(rep.Executed)))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", p.Name, round(p.Duration), extra)
	}
	tw.Flush()
	if rep.WorkDir != "" {
		fmt.Fprintf(w, "\nWork directory: %s\n", rep.WorkDir)
	}
}

func (rep *Report) printTable(w io.Writer) {
	type counts struct{ total, killed, timeout, memory, survived, uncovered, flaky, errors int }
	per := map[mutant.Op]*counts{}
	var sum counts
	for _, o := range rep.Outcomes {
		c := per[o.Op]
		if c == nil {
			c = &counts{}
			per[o.Op] = c
		}
		for _, c := range []*counts{c, &sum} {
			c.total++
			switch o.Status {
			case Killed:
				c.killed++
			case Timeout:
				c.timeout++
			case Memory:
				c.memory++
			case Survived:
				c.survived++
			case NotCovered:
				c.uncovered++
			case Flaky:
				c.flaky++
			case Error:
				c.errors++
			}
		}
	}
	pct := func(a, b int) string {
		if b == 0 {
			return "–"
		}
		return fmt.Sprintf("%.1f %%", 100*float64(a)/float64(b))
	}
	overall := pct
	if rep.Executed < rep.Covered {
		// All uncovered mutants against a sample of the covered ones would
		// give a meaningless ratio.
		overall = func(int, int) string { return "–" }
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "Operator\tTotal\tKilled\tTimeout\tMemory\tSurvived\tUncovered\tFlaky\tErrors\tScore (covered)\tScore (total)\t")
	row := func(name string, c *counts) {
		detected := c.killed + c.timeout + c.memory
		// Flaky mutants and errors say nothing about the tests, so they
		// count in neither direction.
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t\n", name, c.total, c.killed, c.timeout, c.memory,
			c.survived, c.uncovered, c.flaky, c.errors,
			pct(detected, detected+c.survived), overall(detected, c.total-c.errors-c.flaky))
	}
	for _, op := range mutant.AllOps {
		if c := per[op]; c != nil {
			row(string(op), c)
		}
	}
	row("Total", &sum)
	tw.Flush()
}

// printSurvivors lists the survivors by how much they say about the tests:
// whole functions without an effective test first, then untested error
// handling, then the rest, each grouped by function.
func (rep *Report) printSurvivors(w io.Writer, verbose bool) {
	var surv []Outcome
	for _, o := range rep.Outcomes {
		if o.Status == Survived {
			surv = append(surv, o)
		}
	}
	if len(surv) == 0 {
		return
	}
	detailed := verbose || len(surv) <= detailLimit
	sections := []struct {
		title, hint, cat string
	}{
		{"Functions without an effective test", "The function can be emptied without any test failing.", mutant.CatFunc},
		{"Bypassable checks", "A condition that exits early (return, continue, panic) can be removed or weakened without any test failing.", mutant.CatGuard},
		{"Untested error handling", "No test triggers this error path or checks its result.", mutant.CatErr},
		{"Other survivors", "", ""},
	}
	fmt.Fprintf(w, "\nSurviving mutants: %d\n", len(surv))
	for _, sec := range sections {
		var items []Outcome
		for _, o := range surv {
			if o.Category == sec.cat {
				items = append(items, o)
			}
		}
		if len(items) == 0 {
			continue
		}
		groups := groupByFunc(items)
		fmt.Fprintf(w, "\n%s (%d in %d function(s))", sec.title, len(items), len(groups))
		if sec.hint != "" {
			fmt.Fprintf(w, "\n  %s", sec.hint)
		}
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		shown := groups
		if !detailed && len(groups) > 15 {
			shown = groups[:15]
		}
		for _, g := range shown {
			first := g[0]
			where := fmt.Sprintf("%s:%d", rep.rel(first.File), first.Line)
			if sec.cat == mutant.CatFunc {
				var vars []string
				for _, o := range g {
					vars = append(vars, o.Mutated)
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", where, first.Func, strings.Join(vars, ", "))
				continue
			}
			if !detailed {
				fmt.Fprintf(tw, "  %s\t%s\t%d×\tlines %s\n", where, funcLabel(first), len(g), lines(g))
				continue
			}
			tw.Flush()
			fmt.Fprintf(w, "  %s  %s\n", funcLabel(first), rep.rel(first.File))
			for _, o := range g {
				fmt.Fprintf(tw, "    line %d\t%s\t#%d\t%s\n", o.Line, o.Op, o.ID, change(o))
			}
		}
		tw.Flush()
		if len(shown) < len(groups) {
			fmt.Fprintf(w, "  … and %d more functions (-v shows all, -json saves them)\n", len(groups)-len(shown))
		}
	}
}

// groupByFunc groups outcomes by file and function, largest group first.
func groupByFunc(os []Outcome) [][]Outcome {
	idx := map[string]int{}
	var groups [][]Outcome
	for _, o := range os {
		key := o.File + "\x00" + o.Func
		i, ok := idx[key]
		if !ok {
			i = len(groups)
			idx[key] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], o)
	}
	for _, g := range groups {
		slices.SortFunc(g, func(a, b Outcome) int { return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.ID, b.ID)) })
	}
	slices.SortStableFunc(groups, func(a, b []Outcome) int {
		return cmp.Or(-cmp.Compare(len(a), len(b)), cmp.Compare(a[0].File, b[0].File), cmp.Compare(a[0].Line, b[0].Line))
	})
	return groups
}

func funcLabel(o Outcome) string {
	if o.Func == "" {
		return "(package level)"
	}
	return o.Func
}

func lines(g []Outcome) string {
	var ls []string
	last := -1
	for _, o := range g {
		if o.Line != last {
			ls = append(ls, strconv.Itoa(o.Line))
			last = o.Line
		}
	}
	if len(ls) > 8 {
		ls = append(ls[:8], "…")
	}
	return strings.Join(ls, ", ")
}

func change(o Outcome) string {
	if o.Op == mutant.SBR || o.Op == mutant.RET || o.Op == mutant.SEL {
		return o.Mutated + ": " + o.Original
	}
	return o.Original + "  →  " + o.Mutated
}

func (rep *Report) list(w io.Writer, title string, status Status) {
	var os []Outcome
	for _, o := range rep.Outcomes {
		if o.Status == status {
			os = append(os, o)
		}
	}
	if len(os) == 0 {
		return
	}
	sort.SliceStable(os, func(i, j int) bool {
		if os[i].File != os[j].File {
			return os[i].File < os[j].File
		}
		return os[i].Line < os[j].Line
	})
	fmt.Fprintf(w, "\n%s (%d):\n", title, len(os))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, o := range os {
		where := fmt.Sprintf("%s:%d:%d", rep.rel(o.File), o.Line, o.Col)
		detail := ""
		if o.Func != "" {
			detail = "in " + o.Func
		}
		if o.Detail != "" {
			detail += " " + firstLine(o.Detail)
		}
		fmt.Fprintf(tw, "  #%d\t%s\t%s\t%s\t%s\n", o.ID, o.Op, where, change(o), detail)
	}
	tw.Flush()
}

func (rep *Report) rel(path string) string {
	if r, err := filepath.Rel(rep.Dir, path); err == nil {
		return r
	}
	return path
}

// WriteJSON writes the report as JSON.
func (rep *Report) WriteJSON(path string) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func round(d time.Duration) time.Duration {
	switch {
	case d > 10*time.Second:
		return d.Round(100 * time.Millisecond)
	case d > time.Second:
		return d.Round(10 * time.Millisecond)
	default:
		return d.Round(100 * time.Microsecond)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
