package mutant

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"strings"
)

// MC/DC (modified condition/decision coverage) asks for every condition of a
// decision to be shown to affect the outcome on its own: two evaluations in
// which only that condition changes and the outcome changes with it.
//
// A decision is a boolean expression with at least two conditions joined by
// && or ||; conditions are its operands that contain no && , || or ! at the
// top. Each condition is wrapped in a probe that records its value, and the
// decision in a closure that holds the vector of recorded values:
//
//	func() bool { var _mov uint64; return _moD(id, (_moC(&_mov, 0, a) && _moC(&_mov, 1, b)), &_mov) }()
//
// A local vector per evaluation keeps recursion and goroutines apart. Go
// evaluates && and || with short circuit, so a condition that was not
// evaluated matches either value when looking for independence pairs (the
// usual "unique cause with short circuit" rule, as in LLVM and GCC).

// maxConds is the number of conditions a vector can hold.
const maxConds = 32

// Cond is a condition of a decision.
type Cond struct {
	Text string `json:"text"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// Decision is a boolean expression instrumented for MC/DC.
type Decision struct {
	ID      int    `json:"id"`
	Pkg     string `json:"pkg"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	EndLine int    `json:"endLine"`
	Func    string `json:"func,omitempty"`
	Expr    string `json:"expr"`
	Conds   []Cond `json:"conditions"`
	// Reason says why a decision is not measured; empty for measured ones.
	Reason string `json:"reason,omitempty"`
	tree   *condNode
}

// Reasons for leaving a decision out of the measurement.
const (
	SkipNamedBool  = "named bool type"
	SkipRecover    = "calls recover"
	SkipManyConds  = "more than 32 conditions"
	SkipTypeErrors = "probe does not compile"
)

// condNode is the logical structure of a decision. Op is token.LAND,
// token.LOR or token.NOT; a leaf (Op == token.ILLEGAL) is condition cond,
// or a constant with value val if cond < 0.
type condNode struct {
	op   token.Token
	cond int
	val  bool
	x, y *condNode
}

// condRef locates a condition: decision ID and position in the vector.
type condRef struct{ dec, k int }

// Vector is one recorded evaluation of a decision: which conditions were
// evaluated, their values and the outcome.
type Vector struct {
	Eval uint32
	Val  uint32
	Out  bool
}

// ParseVector parses the fields that _moRecordVec writes.
func ParseVector(v uint64, out bool) Vector {
	return Vector{Eval: uint32(v), Val: uint32(v >> 32), Out: out}
}

// isLogical reports whether e is a node of a decision's logical structure.
func isLogical(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.BinaryExpr:
		return e.Op == token.LAND || e.Op == token.LOR
	case *ast.UnaryExpr:
		return e.Op == token.NOT
	case *ast.ParenExpr:
		return isLogical(e.X)
	}
	return false
}

// planDecision registers e as a decision if it is the outermost node of a
// logical expression with at least two conditions. Nested logical nodes are
// marked, so only the outermost one starts a decision; the conditions are
// visited later and may hold decisions of their own, e.g. f(a && b) || c.
// A decision that cannot be instrumented is registered with its reason, so
// the report can tell that it was not measured.
func (g *fileGen) planDecision(e ast.Expr) {
	if !g.mcdc || g.inDecision[e] || !isLogical(e) {
		return
	}
	var leaves []ast.Expr
	var build func(e ast.Expr) *condNode
	build = func(e ast.Expr) *condNode {
		switch n := e.(type) {
		case *ast.ParenExpr:
			if isLogical(n.X) {
				g.inDecision[n] = true
				return build(n.X)
			}
		case *ast.UnaryExpr:
			if n.Op == token.NOT {
				g.inDecision[n] = true
				return &condNode{op: token.NOT, x: build(n.X)}
			}
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				g.inDecision[n] = true
				return &condNode{op: n.Op, x: build(n.X), y: build(n.Y)}
			}
		}
		e = ast.Unparen(e)
		if tv, ok := g.info.Types[e]; ok && tv.Value != nil {
			// A constant condition (a debug flag, say) can never change,
			// so it gets no probe.
			return &condNode{op: token.ILLEGAL, cond: -1, val: tv.Value.Kind() == constant.Bool && constant.BoolVal(tv.Value)}
		}
		leaves = append(leaves, e)
		return &condNode{op: token.ILLEGAL, cond: len(leaves) - 1}
	}
	tree := build(e)
	tv, ok := g.info.Types[e]
	if len(leaves) < 2 || !ok || tv.Value != nil {
		return // no decision, or one that cannot change
	}
	id := *g.nextDec
	*g.nextDec++
	pos, end := g.fset.Position(e.Pos()), g.fset.Position(e.End())
	d := Decision{
		ID: id, Pkg: g.pkg.PkgPath, File: pos.Filename, Line: pos.Line, Col: pos.Column, EndLine: end.Line,
		Func: g.currentFunc(), Expr: g.snippet(e), tree: tree,
	}
	for _, l := range leaves {
		p := g.fset.Position(l.Pos())
		d.Conds = append(d.Conds, Cond{Text: g.snippet(l), Line: p.Line, Col: p.Column})
	}
	switch {
	case !isPlainBool(tv.Type):
		d.Reason = SkipNamedBool // the probe closure returns plain bool
	case len(leaves) > maxConds:
		d.Reason = SkipManyConds
	case g.callsRecover(e):
		d.Reason = SkipRecover
	default:
		for k, l := range leaves {
			g.conds[l] = condRef{id, k}
		}
		g.roots[e] = id
	}
	g.decisions = append(g.decisions, d)
}

// callsRecover reports whether e calls recover, which stops working once it
// is no longer called directly by the deferred function.
func (g *fileGen) callsRecover(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && g.isBuiltin(call.Fun, "recover") {
			found = true
		}
		return !found
	})
	return found
}

// condProbe wraps a condition: _moC(&_mov, k, cond).
func condProbe(e ast.Expr, k int) ast.Expr {
	return &ast.CallExpr{Fun: ast.NewIdent("_moC"), Args: []ast.Expr{
		&ast.UnaryExpr{Op: token.AND, X: ast.NewIdent("_mov")}, intLit(k), e,
	}}
}

// decisionProbe wraps a decision in a closure holding its vector.
func decisionProbe(e ast.Expr, id int) ast.Expr {
	vec := ast.NewIdent("_mov")
	return &ast.CallExpr{Fun: &ast.FuncLit{
		Type: &ast.FuncType{
			Params:  &ast.FieldList{},
			Results: &ast.FieldList{List: []*ast.Field{{Type: ast.NewIdent("bool")}}},
		},
		Body: &ast.BlockStmt{List: []ast.Stmt{
			&ast.DeclStmt{Decl: &ast.GenDecl{Tok: token.VAR, Specs: []ast.Spec{
				&ast.ValueSpec{Names: []*ast.Ident{vec}, Type: ast.NewIdent("uint64")},
			}}},
			// The vector is passed by pointer: Go does not order reading a
			// plain variable argument against the calls in e.
			&ast.ReturnStmt{Results: []ast.Expr{&ast.CallExpr{Fun: ast.NewIdent("_moD"), Args: []ast.Expr{
				intLit(id), paren(e), &ast.UnaryExpr{Op: token.AND, X: ast.NewIdent("_mov")},
			}}}},
		}},
	}}
}

// CondResult is the MC/DC verdict for one condition.
type CondResult struct {
	Cond
	SeenTrue  bool `json:"seenTrue"`
	SeenFalse bool `json:"seenFalse"`
	// Covered: an independence pair was observed.
	Covered bool `json:"covered"`
	// Suggestion describes the evaluation that would complete a pair.
	Suggestion string `json:"suggestion,omitempty"`
}

// DecisionResult is the MC/DC verdict for one decision.
type DecisionResult struct {
	Decision
	Vectors    int          `json:"vectors"` // distinct evaluations observed
	Covered    int          `json:"covered"` // conditions with an independence pair
	Conditions []CondResult `json:"conditionResults"`
}

// Analyze computes MC/DC for a decision from the vectors of all tests.
func Analyze(d Decision, vectors []Vector) DecisionResult {
	res := DecisionResult{Decision: d, Vectors: len(vectors)}
	n := len(d.Conds)
	var candidates []Vector
	if n <= 12 {
		candidates = feasible(d.tree, n)
	}
	for k := range n {
		bit := uint32(1) << k
		c := CondResult{Cond: d.Conds[k]}
		for _, v := range vectors {
			if v.Eval&bit != 0 {
				c.SeenTrue = c.SeenTrue || v.Val&bit != 0
				c.SeenFalse = c.SeenFalse || v.Val&bit == 0
			}
		}
	pairs:
		for i, a := range vectors {
			for _, b := range vectors[i+1:] {
				if isPair(k, a, b) {
					c.Covered = true
					break pairs
				}
			}
		}
		if c.Covered {
			res.Covered++
		} else {
			c.Suggestion = suggest(d, k, vectors, candidates)
		}
		res.Conditions = append(res.Conditions, c)
	}
	return res
}

// isPair reports whether a and b show condition k's independent effect: k
// was evaluated in both with different values, the outcomes differ, and
// every other condition evaluated in both has the same value.
func isPair(k int, a, b Vector) bool {
	bit := uint32(1) << k
	both := a.Eval & b.Eval
	return a.Out != b.Out && both&bit != 0 && (a.Val^b.Val)&both == bit
}

// feasible lists the distinct vectors that the decision can produce.
func feasible(t *condNode, n int) []Vector {
	seen := map[Vector]bool{}
	var out []Vector
	for assign := range uint32(1) << n {
		var v Vector
		v.Out = eval(t, assign, &v)
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func eval(t *condNode, assign uint32, v *Vector) bool {
	switch t.op {
	case token.LAND:
		return eval(t.x, assign, v) && eval(t.y, assign, v)
	case token.LOR:
		return eval(t.x, assign, v) || eval(t.y, assign, v)
	case token.NOT:
		return !eval(t.x, assign, v)
	}
	if t.cond < 0 {
		return t.val
	}
	bit := uint32(1) << t.cond
	v.Eval |= bit
	v.Val |= assign & bit
	return assign&bit != 0
}

// suggest proposes the missing evaluation: preferably one that forms a pair
// with an observed vector, otherwise a complete pair.
func suggest(d Decision, k int, observed, candidates []Vector) string {
	for _, o := range observed {
		for _, c := range candidates {
			if isPair(k, o, c) {
				return formatVector(d, c)
			}
		}
	}
	for i, a := range candidates {
		for _, b := range candidates[i+1:] {
			if isPair(k, a, b) {
				return formatVector(d, a) + " and " + formatVector(d, b)
			}
		}
	}
	return ""
}

func formatVector(d Decision, v Vector) string {
	var parts []string
	for k, c := range d.Conds {
		bit := uint32(1) << k
		if v.Eval&bit != 0 {
			parts = append(parts, fmt.Sprintf("%s = %t", short(c.Text, 40), v.Val&bit != 0))
		}
	}
	return strings.Join(parts, ", ") + fmt.Sprintf(" → %t", v.Out)
}

func short(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
