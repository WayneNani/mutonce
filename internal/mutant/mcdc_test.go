package mutant

import (
	"go/token"
	"testing"
)

func leaf(k int) *condNode { return &condNode{op: token.ILLEGAL, cond: k} }

func node(op token.Token, x, y *condNode) *condNode { return &condNode{op: op, x: x, y: y} }

// vec builds a vector from per-condition values: 'T', 'F' or '-' (not evaluated).
func vec(vals string, out bool) Vector {
	var v Vector
	for k, c := range vals {
		if c == '-' {
			continue
		}
		v.Eval |= 1 << k
		if c == 'T' {
			v.Val |= 1 << k
		}
	}
	v.Out = out
	return v
}

func decision(tree *condNode, conds ...string) Decision {
	d := Decision{tree: tree}
	for _, c := range conds {
		d.Conds = append(d.Conds, Cond{Text: c})
	}
	return d
}

func TestAnalyzeAnd(t *testing.T) {
	d := decision(node(token.LAND, leaf(0), leaf(1)), "a", "b")
	res := Analyze(d, []Vector{vec("TT", true), vec("F-", false)})
	if !res.Conditions[0].Covered {
		t.Errorf("a: pair TT/F- not found")
	}
	b := res.Conditions[1]
	if b.Covered || !b.SeenTrue || b.SeenFalse {
		t.Errorf("b: %+v, want uncovered, only true seen", b)
	}
	if want := "a = true, b = false → false"; b.Suggestion != want {
		t.Errorf("b: suggestion %q, want %q", b.Suggestion, want)
	}
	if res.Covered != 1 || res.Vectors != 2 {
		t.Errorf("covered %d of 2 with %d vectors", res.Covered, res.Vectors)
	}
}

// With short circuit, a condition evaluated in only one vector matches
// either value.
func TestAnalyzeOrShortCircuit(t *testing.T) {
	d := decision(node(token.LOR, leaf(0), leaf(1)), "a", "b")
	res := Analyze(d, []Vector{vec("T-", true), vec("FT", true), vec("FF", false)})
	if res.Covered != 2 {
		t.Errorf("covered %d of 2: %+v", res.Covered, res.Conditions)
	}
}

// Two vectors that differ in more than the condition itself are no pair.
func TestAnalyzeUniqueCause(t *testing.T) {
	// (a || b) && c
	d := decision(node(token.LAND, node(token.LOR, leaf(0), leaf(1)), leaf(2)), "a", "b", "c")
	res := Analyze(d, []Vector{vec("T-T", true), vec("FTF", false)})
	if res.Covered != 0 {
		t.Errorf("a and c change together, yet %d covered: %+v", res.Covered, res.Conditions)
	}
	if want := "a = false, b = false → false"; res.Conditions[0].Suggestion != want {
		t.Errorf("a: suggestion %q, want %q", res.Conditions[0].Suggestion, want)
	}
}

func TestAnalyzeNot(t *testing.T) {
	// !(a && b) || c
	d := decision(node(token.LOR, node(token.NOT, node(token.LAND, leaf(0), leaf(1)), nil), leaf(2)), "a", "b", "c")
	res := Analyze(d, []Vector{vec("F--", true), vec("TTF", false), vec("TF-", true), vec("TTT", true)})
	if res.Covered != 3 {
		t.Errorf("covered %d of 3: %+v", res.Covered, res.Conditions)
	}
}

// A decision no test reached gets a complete pair as suggestion.
func TestAnalyzeNeverEvaluated(t *testing.T) {
	d := decision(node(token.LAND, leaf(0), leaf(1)), "a", "b")
	res := Analyze(d, nil)
	if res.Covered != 0 || res.Conditions[0].Suggestion == "" {
		t.Fatalf("%+v", res)
	}
	if want := "a = false → false and a = true, b = true → true"; res.Conditions[0].Suggestion != want {
		t.Errorf("suggestion %q, want %q", res.Conditions[0].Suggestion, want)
	}
}

// A constant operand keeps its value when feasible vectors are enumerated.
func TestFeasibleConstant(t *testing.T) {
	// false || a && b: the constant never short-circuits.
	tree := node(token.LOR, &condNode{op: token.ILLEGAL, cond: -1}, node(token.LAND, leaf(0), leaf(1)))
	got := map[Vector]bool{}
	for _, v := range feasible(tree, 2) {
		got[v] = true
	}
	for _, want := range []Vector{vec("F-", false), vec("TF", false), vec("TT", true)} {
		if !got[want] {
			t.Errorf("missing feasible vector %+v in %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("%d feasible vectors, want 3", len(got))
	}
}

func TestGenerateDecisions(t *testing.T) {
	res := generateExample(t, "./...")
	got := map[string]Decision{}
	for _, d := range res.Decisions {
		got[d.Func] = d
	}
	for fn, want := range map[string][]string{
		"FreeShipping": {"premium", "total > 2500"},
		"Positive":     {"n != nil", "n.Val > 0"},
		"Known":        {"s == Open", "s == Closed"},
	} {
		d, ok := got[fn]
		if !ok {
			t.Errorf("%s: no decision", fn)
			continue
		}
		if len(d.Conds) != len(want) {
			t.Errorf("%s: conditions %+v, want %v", fn, d.Conds, want)
			continue
		}
		for i, c := range d.Conds {
			if c.Text != want[i] {
				t.Errorf("%s: condition %d is %q, want %q", fn, i, c.Text, want[i])
			}
		}
	}
	// Both combines a named bool type, which the probes cannot return: it
	// is reported as not measured instead.
	if _, ok := got["Both"]; ok {
		t.Errorf("decision of a named bool type instrumented")
	}
	if len(res.SkippedDecisions) != 1 || res.SkippedDecisions[0].Func != "Both" || res.SkippedDecisions[0].Reason != SkipNamedBool {
		t.Errorf("skipped decisions: %+v", res.SkippedDecisions)
	}
}
