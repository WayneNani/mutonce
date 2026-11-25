package run

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"

	"github.com/WayneNani/mutonce/internal/mutant"
)

// printMCDC summarises MC/DC per package and lists the decisions in which a
// condition's independent effect was not shown, with the missing test case.
func (rep *Report) printMCDC(w io.Writer, verbose bool) {
	if rep.MCDC == nil {
		if rep.MCDCExcluded > 0 {
			fmt.Fprintf(w, "\nMC/DC: no decisions left, %d dropped by the type check\n", rep.MCDCExcluded)
		}
		return
	}
	type counts struct{ decisions, never, conds, covered int }
	per := map[string]*counts{}
	var pkgs []string
	var sum counts
	var gaps []mutant.DecisionResult
	for _, d := range rep.MCDC {
		c := per[d.Pkg]
		if c == nil {
			c = &counts{}
			per[d.Pkg] = c
			pkgs = append(pkgs, d.Pkg)
		}
		for _, c := range []*counts{c, &sum} {
			c.decisions++
			c.conds += len(d.Conditions)
			c.covered += d.Covered
			if d.Vectors == 0 {
				c.never++
			}
		}
		if d.Covered < len(d.Conditions) {
			gaps = append(gaps, d)
		}
	}
	pct := func(a, b int) string {
		if b == 0 {
			return "–"
		}
		return fmt.Sprintf("%.1f %%", 100*float64(a)/float64(b))
	}
	fmt.Fprintf(w, "\nMC/DC: %d of %d conditions (%s) in %d decisions", sum.covered, sum.conds, pct(sum.covered, sum.conds), sum.decisions)
	if sum.never > 0 {
		fmt.Fprintf(w, ", %d decision(s) never evaluated", sum.never)
	}
	if rep.MCDCExcluded > 0 {
		fmt.Fprintf(w, ", %d dropped by the type check", rep.MCDCExcluded)
	}
	fmt.Fprintln(w)
	if len(pkgs) > 1 {
		slices.Sort(pkgs)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(tw, "Package\tDecisions\tNever evaluated\tConditions\tIndependent\tMC/DC\t")
		for _, p := range pkgs {
			c := per[p]
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t\n", p, c.decisions, c.never, c.conds, c.covered, pct(c.covered, c.conds))
		}
		tw.Flush()
	}
	if len(gaps) == 0 {
		return
	}
	slices.SortFunc(gaps, func(a, b mutant.DecisionResult) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Col, b.Col))
	})
	shown := gaps
	if !verbose && len(gaps) > detailLimit {
		shown = gaps[:detailLimit]
	}
	fmt.Fprintf(w, "\nConditions without a shown independent effect (%d decision(s))\n", len(gaps))
	fmt.Fprintln(w, "  No pair of test runs changes only this condition and with it the outcome.")
	for _, d := range shown {
		fn := d.Func
		if fn == "" {
			fn = "(package level)"
		}
		fmt.Fprintf(w, "  %s:%d  %s  %s\n", rep.rel(d.File), d.Line, fn, d.Expr)
		if d.Vectors == 0 {
			fmt.Fprintln(w, "    never evaluated by a passing test")
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, c := range d.Conditions {
			if c.Covered {
				continue
			}
			missing := ""
			if c.Suggestion != "" {
				missing = "missing: " + c.Suggestion
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s\n", c.Text, seen(c), missing)
		}
		tw.Flush()
	}
	if len(shown) < len(gaps) {
		fmt.Fprintf(w, "  … and %d more decisions (-v shows all, -json saves them)\n", len(gaps)-len(shown))
	}
}

func seen(c mutant.CondResult) string {
	switch {
	case c.SeenTrue && c.SeenFalse:
		return "both values seen"
	case c.SeenTrue:
		return "only true seen"
	case c.SeenFalse:
		return "only false seen"
	}
	return "never evaluated"
}
