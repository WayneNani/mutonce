package run

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"

	"github.com/WayneNani/mutonce/internal/mutant"
)

// printMCDC summarises MC/DC per package, lists the decisions in which a
// condition's independent effect was not shown, with the missing test case,
// and the decisions that could not be measured, with the reason.
func (rep *Report) printMCDC(w io.Writer, verbose bool) {
	total := rep.MCDCTotal
	if total == nil {
		return
	}
	pct := func(s MCDCSummary) string {
		if s.Conditions == 0 {
			return "–"
		}
		return fmt.Sprintf("%.1f %%", s.Percent)
	}
	fmt.Fprintf(w, "\nMC/DC: %d of %d conditions (%s) in %d decisions", total.Covered, total.Conditions, pct(*total), total.Decisions)
	if total.NeverEvaluated > 0 {
		fmt.Fprintf(w, ", %d never evaluated", total.NeverEvaluated)
	}
	if total.NotMeasured > 0 {
		fmt.Fprintf(w, ", %d not measured", total.NotMeasured)
	}
	fmt.Fprintln(w)
	if len(rep.MCDCPackages) > 1 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(tw, "Package\tDecisions\tNever evaluated\tNot measured\tConditions\tIndependent\tMC/DC\t")
		for _, s := range rep.MCDCPackages {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%s\t\n", s.Pkg, s.Decisions, s.NeverEvaluated, s.NotMeasured, s.Conditions, s.Covered, pct(s))
		}
		tw.Flush()
	}
	byPos := func(a, b mutant.Decision) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Col, b.Col))
	}
	var gaps []mutant.DecisionResult
	for _, d := range rep.MCDC {
		if d.Covered < len(d.Conditions) {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) > 0 {
		slices.SortFunc(gaps, func(a, b mutant.DecisionResult) int { return byPos(a.Decision, b.Decision) })
		shown := gaps
		if !verbose && len(gaps) > detailLimit {
			shown = gaps[:detailLimit]
		}
		fmt.Fprintf(w, "\nConditions without a shown independent effect (%d decision(s))\n", len(gaps))
		fmt.Fprintln(w, "  No pair of test runs changes only this condition and with it the outcome.")
		for _, d := range shown {
			fmt.Fprintf(w, "  %s:%d  %s  %s\n", rep.rel(d.File), d.Line, decisionFunc(d.Decision), d.Expr)
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
	if len(rep.MCDCSkipped) > 0 {
		skipped := slices.Clone(rep.MCDCSkipped)
		slices.SortFunc(skipped, byPos)
		shown := skipped
		if !verbose && len(skipped) > detailLimit {
			shown = skipped[:detailLimit]
		}
		fmt.Fprintf(w, "\nDecisions not measured (%d)\n", len(skipped))
		fmt.Fprintln(w, "  Their conditions count as not covered.")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, d := range shown {
			fmt.Fprintf(tw, "  %s:%d\t%s\t%s\tleft out: %s\n", rep.rel(d.File), d.Line, decisionFunc(d), d.Expr, d.Reason)
		}
		tw.Flush()
		if len(shown) < len(skipped) {
			fmt.Fprintf(w, "  … and %d more (-v shows all, -json saves them)\n", len(skipped)-len(shown))
		}
	}
}

func decisionFunc(d mutant.Decision) string {
	if d.Func == "" {
		return "(package level)"
	}
	return d.Func
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

// MCDCSummary counts MC/DC for one package or, with an empty Pkg, for the
// whole run. The conditions of decisions that were not measured count as
// not covered, so 100 % means every decision was measured and covered.
type MCDCSummary struct {
	Pkg            string  `json:"pkg,omitempty"`
	Decisions      int     `json:"decisions"`
	NotMeasured    int     `json:"notMeasured"` // listed in mcdcSkipped
	NeverEvaluated int     `json:"neverEvaluated"`
	Conditions     int     `json:"conditions"`
	Covered        int     `json:"covered"`
	Percent        float64 `json:"percent"` // 100 without any condition
}

func (s *MCDCSummary) finish() {
	s.Percent = 100
	if s.Conditions > 0 {
		s.Percent = 100 * float64(s.Covered) / float64(s.Conditions)
	}
}

func (rep *Report) summarizeMCDC() {
	per := map[string]*MCDCSummary{}
	total := &MCDCSummary{}
	add := func(pkg string, f func(*MCDCSummary)) {
		s := per[pkg]
		if s == nil {
			s = &MCDCSummary{Pkg: pkg}
			per[pkg] = s
		}
		f(s)
		f(total)
	}
	for _, d := range rep.MCDC {
		add(d.Pkg, func(s *MCDCSummary) {
			s.Decisions++
			s.Conditions += len(d.Conditions)
			s.Covered += d.Covered
			if d.Vectors == 0 {
				s.NeverEvaluated++
			}
		})
	}
	for _, d := range rep.MCDCSkipped {
		add(d.Pkg, func(s *MCDCSummary) {
			s.Decisions++
			s.NotMeasured++
			s.Conditions += len(d.Conds)
		})
	}
	rep.MCDCPackages = nil
	for _, s := range per {
		s.finish()
		rep.MCDCPackages = append(rep.MCDCPackages, *s)
	}
	slices.SortFunc(rep.MCDCPackages, func(a, b MCDCSummary) int { return cmp.Compare(a.Pkg, b.Pkg) })
	total.finish()
	rep.MCDCTotal = total
}

// MCDCBelow reports whether MC/DC was measured and the total is below min
// percent.
func (rep *Report) MCDCBelow(min float64) bool {
	return rep.MCDCTotal != nil && rep.MCDCTotal.Percent < min
}
