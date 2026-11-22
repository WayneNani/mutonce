package mutant

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/packages"
)

// site is a planned mutation point. One site can hold several variants, each
// of which is a mutant with its own ID.
type site struct {
	op   Op
	ids  []int
	vars []string
	typ  string // AOR: result type spelled in the scope of the file
}

func (s *site) add(id int, v string) {
	s.ids = append(s.ids, id)
	s.vars = append(s.vars, v)
}

func (s *site) allExcluded(excluded map[int]bool) bool {
	for _, id := range s.ids {
		if !excluded[id] {
			return false
		}
	}
	return true
}

// fileGen plans and applies the mutations of one file.
type fileGen struct {
	pkg      *packages.Package
	info     *types.Info
	fset     *token.FileSet
	file     *ast.File
	filename string
	src      []byte
	ops      map[Op]bool
	next     *int

	plan    map[ast.Node][]*site // a select can be both removed (SBR) and lose its default (SEL)
	mutants []Mutant
	imports map[string]string // import path -> qualifier usable in this file

	funcNames []string
	funcTypes []*ast.FuncType
	arid      int // > 0 while inside code that is not worth mutating
}

func newFileGen(pkg *packages.Package, file *ast.File, src []byte, ops map[Op]bool, next *int) *fileGen {
	g := &fileGen{
		pkg:      pkg,
		info:     pkg.TypesInfo,
		fset:     pkg.Fset,
		file:     file,
		filename: pkg.Fset.File(file.Pos()).Name(),
		src:      src,
		ops:      ops,
		next:     next,
		plan:     map[ast.Node][]*site{},
		imports:  map[string]string{},
	}
	for _, spec := range file.Imports {
		pn := g.info.PkgNameOf(spec)
		if pn == nil {
			continue
		}
		switch name := pn.Name(); name {
		case "_":
		case ".":
			g.imports[pn.Imported().Path()] = ""
		default:
			g.imports[pn.Imported().Path()] = name
		}
	}
	return g
}

// collect walks the file in source order and assigns mutant IDs. The order
// only depends on the original source, so IDs stay stable across rounds.
func (g *fileGen) collect() {
	var stack []ast.Node
	var aridMarks []bool
	ast.Inspect(g.file, func(n ast.Node) bool {
		if n == nil {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if aridMarks[len(aridMarks)-1] {
				g.arid--
			}
			aridMarks = aridMarks[:len(aridMarks)-1]
			switch top.(type) {
			case *ast.FuncDecl, *ast.FuncLit:
				g.funcNames = g.funcNames[:len(g.funcNames)-1]
				g.funcTypes = g.funcTypes[:len(g.funcTypes)-1]
			}
			return true
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		arid := g.isArid(n, parent)
		if arid {
			g.arid++
		}
		stack = append(stack, n)
		aridMarks = append(aridMarks, arid)

		switch n := n.(type) {
		case *ast.FuncDecl:
			g.funcNames = append(g.funcNames, g.funcName(n))
			g.funcTypes = append(g.funcTypes, n.Type)
			g.planRET(n)
		case *ast.FuncLit:
			g.funcNames = append(g.funcNames, g.currentFunc()+".func")
			g.funcTypes = append(g.funcTypes, n.Type)
		case *ast.BlockStmt:
			body, decl := isFuncBody(n, parent)
			g.planStmts(n.List, body, decl)
		case *ast.CaseClause:
			g.planStmts(n.Body, false, false)
		case *ast.CommClause:
			g.planStmts(n.Body, false, false)
		case *ast.BinaryExpr:
			g.planBinary(n, inGuard(stack, n))
		case *ast.ReturnStmt:
			g.planERR(n)
		case *ast.SelectStmt:
			g.planSEL(n)
		}
		return true
	})
}

func (g *fileGen) add(op Op, at ast.Node, orig, mutated string) int {
	return g.addCat(op, at, orig, mutated, "")
}

// addCat registers a mutant. at spans the code the mutant changes; the diff
// mode selects mutants whose span touches a changed line.
func (g *fileGen) addCat(op Op, at ast.Node, orig, mutated, cat string) int {
	id := *g.next
	*g.next++
	pos := g.fset.Position(at.Pos())
	g.mutants = append(g.mutants, Mutant{
		ID:       id,
		Op:       op,
		Pkg:      g.pkg.PkgPath,
		File:     pos.Filename,
		Line:     pos.Line,
		Col:      pos.Column,
		EndLine:  g.fset.Position(at.End()).Line,
		Func:     g.currentFunc(),
		Category: cat,
		Original: orig,
		Mutated:  mutated,
	})
	return id
}

func (g *fileGen) planRET(fd *ast.FuncDecl) {
	if !g.ops[RET] || g.arid > 0 || fd.Body == nil || len(fd.Body.List) == 0 || fd.Name.Name == "_" {
		return
	}
	orig := "func " + g.currentFunc()
	s := &site{op: RET}
	results := fd.Type.Results
	if results == nil || len(results.List) == 0 {
		s.add(g.addCat(RET, fd, orig, "body removed", CatFunc), "zero")
	} else {
		// A function that returns nothing but zero values already is its
		// own zero mutant; only injecting an error can change it.
		if !g.returnsOnlyZero(fd.Body) {
			s.add(g.addCat(RET, fd, orig, "returns zero values", CatFunc), "zero")
		}
		last := results.List[len(results.List)-1]
		if isErrorType(g.info.TypeOf(last.Type)) {
			s.add(g.addCat(RET, fd, orig, "returns an error", CatFunc), "err")
		}
	}
	if len(s.ids) > 0 {
		g.plan[fd] = append(g.plan[fd], s)
	}
}

// returnsOnlyZero reports whether body is a single return of zero values
// (nil, 0, "", false), like `func (f *File) Close() error { return nil }`.
func (g *fileGen) returnsOnlyZero(body *ast.BlockStmt) bool {
	if len(body.List) != 1 {
		return false
	}
	r, ok := body.List[0].(*ast.ReturnStmt)
	if !ok || len(r.Results) == 0 {
		return false
	}
	for _, e := range r.Results {
		tv, ok := g.info.Types[e]
		if !ok {
			return false
		}
		switch {
		case tv.IsNil():
		case tv.Value == nil:
			return false
		case tv.Value.Kind() == constant.Bool:
			if constant.BoolVal(tv.Value) {
				return false
			}
		case tv.Value.Kind() == constant.String:
			if constant.StringVal(tv.Value) != "" {
				return false
			}
		default:
			if constant.Sign(tv.Value) != 0 {
				return false
			}
		}
	}
	return true
}

// exclusiveEquals reports whether x and y are `v == A` and `v == B` with the
// same v and different constants: both cannot hold at once, so `x != y` is
// equivalent to `x || y`.
func (g *fileGen) exclusiveEquals(x, y ast.Expr) bool {
	a, ok1 := ast.Unparen(x).(*ast.BinaryExpr)
	b, ok2 := ast.Unparen(y).(*ast.BinaryExpr)
	if !ok1 || !ok2 || a.Op != token.EQL || b.Op != token.EQL || g.text(a.X) != g.text(b.X) {
		return false
	}
	va, vb := g.info.Types[a.Y].Value, g.info.Types[b.Y].Value
	return va != nil && vb != nil && !constant.Compare(va, token.EQL, vb)
}

// planERR swallows the error of `return …, err` in functions whose last
// result is an error: the caller then carries on as if all went well.
func (g *fileGen) planERR(r *ast.ReturnStmt) {
	if !g.ops[ERR] || g.arid > 0 || len(r.Results) == 0 || !g.returnsError() {
		return
	}
	ft := g.funcTypes[len(g.funcTypes)-1]
	if len(r.Results) != ft.Results.NumFields() {
		return // return f() with a multi-value call
	}
	last := r.Results[len(r.Results)-1]
	if tv, ok := g.info.Types[last]; !ok || tv.IsNil() {
		return
	}
	parts := make([]string, len(r.Results))
	for i, e := range r.Results[:len(r.Results)-1] {
		parts[i] = g.text(e)
	}
	parts[len(parts)-1] = "nil"
	s := &site{op: ERR}
	s.add(g.addCat(ERR, r, g.snippet(r), "return "+strings.Join(parts, ", "), CatErr), "nil")
	g.plan[last] = append(g.plan[last], s)
}

// planSEL removes the default clause of a select, so it blocks until a
// channel is ready instead of falling through.
func (g *fileGen) planSEL(sel *ast.SelectStmt) {
	if !g.ops[SEL] || g.arid > 0 {
		return
	}
	for _, c := range sel.Body.List {
		if cc, ok := c.(*ast.CommClause); ok && cc.Comm == nil {
			s := &site{op: SEL}
			s.add(g.add(SEL, cc, "select { … default: … }", "default clause removed"), "")
			g.plan[sel] = append(g.plan[sel], s)
			return
		}
	}
}

// returnsError reports whether the innermost enclosing function has an
// error as its last result.
func (g *fileGen) returnsError() bool {
	if len(g.funcTypes) == 0 {
		return false
	}
	ft := g.funcTypes[len(g.funcTypes)-1]
	if ft.Results == nil || len(ft.Results.List) == 0 {
		return false
	}
	return isErrorType(g.info.TypeOf(ft.Results.List[len(ft.Results.List)-1].Type))
}

// isErrorCheck reports whether e compares an error with nil.
func (g *fileGen) isErrorCheck(e ast.Expr) bool {
	be, ok := ast.Unparen(e).(*ast.BinaryExpr)
	if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
		return false
	}
	isNil := func(e ast.Expr) bool { tv, ok := g.info.Types[e]; return ok && tv.IsNil() }
	return isNil(be.Y) && isErrorType(g.info.TypeOf(be.X)) || isNil(be.X) && isErrorType(g.info.TypeOf(be.Y))
}

// isGuard reports whether s is a guard clause: `if cond { …; return }`
// without else, ending in return, break, continue, goto or panic.
func isGuard(s *ast.IfStmt) bool {
	if s.Else != nil || len(s.Body.List) == 0 {
		return false
	}
	switch last := s.Body.List[len(s.Body.List)-1].(type) {
	case *ast.ReturnStmt, *ast.BranchStmt:
		return true
	case *ast.ExprStmt:
		call, ok := last.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := call.Fun.(*ast.Ident)
		return ok && id.Name == "panic"
	}
	return false
}

// inGuard reports whether n is part of the condition of a guard clause.
// The innermost enclosing statement decides: a comparison in the body of
// the if belongs to that body, not to the guard.
func inGuard(stack []ast.Node, n ast.Node) bool {
	for i := len(stack) - 1; i >= 0; i-- {
		s, ok := stack[i].(ast.Stmt)
		if !ok {
			continue
		}
		ifs, ok := s.(*ast.IfStmt)
		return ok && ifs.Cond.Pos() <= n.Pos() && n.End() <= ifs.Cond.End() && isGuard(ifs)
	}
	return false
}

// sbrCategory marks removed statements that belong to error handling:
// `return …, err` and `if err != nil { … }`.
func (g *fileGen) sbrCategory(s ast.Stmt) string {
	switch s := s.(type) {
	case *ast.ReturnStmt:
		if len(s.Results) > 0 && g.returnsError() {
			if tv, ok := g.info.Types[s.Results[len(s.Results)-1]]; ok && !tv.IsNil() {
				return CatErr
			}
		}
	case *ast.IfStmt:
		if g.isErrorCheck(s.Cond) {
			return CatErr
		}
		if isGuard(s) {
			return CatGuard
		}
	}
	return ""
}

func (g *fileGen) planStmts(list []ast.Stmt, funcBody, funcDecl bool) {
	if !g.ops[SBR] || g.arid > 0 {
		return
	}
	var ft *ast.FuncType
	if len(g.funcTypes) > 0 {
		ft = g.funcTypes[len(g.funcTypes)-1]
	}
	hasResults := ft != nil && ft.Results != nil && len(ft.Results.List) > 0
	for i, s := range list {
		if !g.sbrEligible(s) {
			continue
		}
		if funcBody && i == len(list)-1 {
			if hasResults {
				continue // removing it would leave a missing return
			}
			if r, ok := s.(*ast.ReturnStmt); ok && len(r.Results) == 0 {
				continue // equivalent mutant
			}
		}
		if funcDecl && !hasResults && len(list) == 1 && g.ops[RET] {
			continue // identical to the RET mutant of this function
		}
		st := &site{op: SBR}
		st.add(g.addCat(SBR, s, g.snippet(s), "statement removed", g.sbrCategory(s)), "")
		g.plan[s] = append(g.plan[s], st)
	}
}

func (g *fileGen) sbrEligible(s ast.Stmt) bool {
	switch s := s.(type) {
	case *ast.ExprStmt:
		call, ok := ast.Unparen(s.X).(*ast.CallExpr)
		return !ok || !g.isAridCall(call)
	case *ast.DeferStmt:
		return !g.isAridCall(s.Call)
	case *ast.GoStmt:
		return !g.isAridCall(s.Call)
	case *ast.AssignStmt:
		return s.Tok != token.DEFINE
	case *ast.IncDecStmt, *ast.SendStmt, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt,
		*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.ReturnStmt, *ast.BlockStmt:
		return true
	}
	// Declarations, labels and branch statements cannot be wrapped in a block.
	return false
}

func (g *fileGen) planBinary(n *ast.BinaryExpr, guard bool) {
	if g.arid > 0 {
		return
	}
	tv, ok := g.info.Types[n]
	if !ok || tv.Value != nil {
		return // constant expressions cannot hold a runtime switch
	}
	x, y := g.text(n.X), g.text(n.Y)
	orig := g.snippet(n)
	switch n.Op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		if !g.ops[ROR] || !isPlainBool(tv.Type) {
			return
		}
		vars := rorTable[n.Op]
		if (n.Op == token.EQL || n.Op == token.NEQ) &&
			(!isOrdered(g.info.TypeOf(n.X)) || !isOrdered(g.info.TypeOf(n.Y)) || g.isNamedConst(n.X) || g.isNamedConst(n.Y)) {
			vars = rorUnordered
		}
		equivalent := g.equivalentROR(n)
		cat := ""
		switch {
		case g.isErrorCheck(n):
			cat = CatErr
		case guard:
			cat = CatGuard
		}
		s := &site{op: ROR}
		for _, v := range vars {
			if v == equivalent {
				continue
			}
			desc := v
			if _, isOp := opTokens[v]; isOp {
				desc = x + " " + v + " " + y
			}
			s.add(g.addCat(ROR, n, orig, desc, cat), v)
		}
		g.plan[n] = append(g.plan[n], s)
	case token.LAND, token.LOR:
		if !g.ops[LCR] || !isPlainBool(tv.Type) {
			return
		}
		s := &site{op: LCR}
		for _, v := range lcrTable[n.Op] {
			if v == "!=" && g.exclusiveEquals(n.X, n.Y) {
				continue
			}
			var desc string
			switch v {
			case "a":
				desc = x
			case "b":
				desc = y
			case "==", "!=":
				desc = x + " " + v + " " + y
			default:
				desc = v
			}
			cat := ""
			if guard {
				cat = CatGuard
			}
			s.add(g.addCat(LCR, n, orig, desc, cat), v)
		}
		g.plan[n] = append(g.plan[n], s)
	case token.ADD, token.SUB, token.MUL, token.QUO, token.REM:
		if !g.ops[AOR] {
			return
		}
		b, ok := tv.Type.Underlying().(*types.Basic)
		if !ok || b.Info()&types.IsNumeric == 0 || b.Info()&types.IsUntyped != 0 {
			return
		}
		typ, ok := g.spell(tv.Type)
		if !ok {
			return
		}
		mop := aorTable[n.Op].String()
		s := &site{op: AOR, typ: typ}
		s.add(g.add(AOR, n, orig, x+" "+mop+" "+y), mop)
		g.plan[n] = append(g.plan[n], s)
	}
}

// Comparisons of a value that cannot be negative with its minimum (0 or "")
// have one trivially equivalent ROR variant, e.g. len(s) == 0 → len(s) <= 0.
var (
	equivZeroRight = map[token.Token]string{token.NEQ: ">", token.EQL: "<=", token.GTR: "!=", token.LEQ: "=="}
	equivZeroLeft  = map[token.Token]string{token.NEQ: "<", token.EQL: ">=", token.LSS: "!=", token.GEQ: "=="}
)

func (g *fileGen) equivalentROR(n *ast.BinaryExpr) string {
	switch {
	case g.isMinimum(n.Y) && g.nonNegative(n.X):
		return equivZeroRight[n.Op]
	case g.isMinimum(n.X) && g.nonNegative(n.Y):
		return equivZeroLeft[n.Op]
	}
	return ""
}

// isNamedConst reports whether e names a declared constant. Go has no enum
// type, so `status != Locked` is ordered for the type checker but
// categorical in meaning: variants like `status > Locked` are noise that no
// sensible test pins down.
func (g *fileGen) isNamedConst(e ast.Expr) bool {
	var id *ast.Ident
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	default:
		return false
	}
	_, ok := g.info.Uses[id].(*types.Const)
	return ok
}

func (g *fileGen) isMinimum(e ast.Expr) bool {
	tv, ok := g.info.Types[e]
	if !ok || tv.Value == nil {
		return false
	}
	switch tv.Value.Kind() {
	case constant.Int:
		return constant.Sign(tv.Value) == 0
	case constant.String:
		return constant.StringVal(tv.Value) == ""
	}
	return false
}

func (g *fileGen) nonNegative(e ast.Expr) bool {
	if call, ok := ast.Unparen(e).(*ast.CallExpr); ok && (g.isBuiltin(call.Fun, "len") || g.isBuiltin(call.Fun, "cap")) {
		return true
	}
	t := g.info.TypeOf(e)
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&(types.IsUnsigned|types.IsString) != 0
}

// isArid reports whether n starts code whose mutation would not help anyone:
// logging, sleeping, metrics and capacity hints (after Google's arid nodes).
func (g *fileGen) isArid(n, parent ast.Node) bool {
	if call, ok := n.(*ast.CallExpr); ok && g.isAridCall(call) {
		return true
	}
	if call, ok := parent.(*ast.CallExpr); ok && len(call.Args) == 3 && call.Args[2] == n && g.isBuiltin(call.Fun, "make") {
		return true
	}
	return false
}

var aridPkgPrefixes = []string{
	"go.uber.org/zap",
	"github.com/sirupsen/logrus",
	"github.com/rs/zerolog",
	"github.com/prometheus/client_golang",
}

func (g *fileGen) isAridCall(call *ast.CallExpr) bool {
	var obj types.Object
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.SelectorExpr:
		obj = g.info.Uses[f.Sel]
	case *ast.Ident:
		obj = g.info.Uses[f]
	}
	fn, ok := obj.(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	path, name := fn.Pkg().Path(), fn.Name()
	if strings.HasPrefix(name, "Fatal") || strings.HasPrefix(name, "Panic") {
		return false // these change control flow
	}
	switch path {
	case "fmt":
		return strings.HasPrefix(name, "Print")
	case "time":
		return name == "Sleep"
	case "log", "log/slog":
		return true
	}
	for _, p := range aridPkgPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func (g *fileGen) isBuiltin(fun ast.Expr, name string) bool {
	id, ok := ast.Unparen(fun).(*ast.Ident)
	if !ok {
		return false
	}
	b, ok := g.info.Uses[id].(*types.Builtin)
	return ok && b.Name() == name
}

// spell returns the type as it can be written in this file.
func (g *fileGen) spell(t types.Type) (string, bool) {
	ok := true
	s := types.TypeString(t, func(p *types.Package) string {
		if p == g.pkg.Types {
			return ""
		}
		if q, found := g.imports[p.Path()]; found {
			return q
		}
		ok = false
		return p.Name()
	})
	return s, ok
}

func (g *fileGen) funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return "(" + g.text(fd.Recv.List[0].Type) + ")." + fd.Name.Name
}

func (g *fileGen) currentFunc() string {
	if len(g.funcNames) == 0 {
		return ""
	}
	return g.funcNames[len(g.funcNames)-1]
}

func (g *fileGen) text(n ast.Node) string {
	tf := g.fset.File(n.Pos())
	return string(g.src[tf.Offset(n.Pos()):tf.Offset(n.End())])
}

// snippet is the first line of the node's source.
func (g *fileGen) snippet(n ast.Node) string {
	s := g.text(n)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

func isFuncBody(b *ast.BlockStmt, parent ast.Node) (body, decl bool) {
	switch p := parent.(type) {
	case *ast.FuncDecl:
		return p.Body == b, p.Body == b
	case *ast.FuncLit:
		return p.Body == b, false
	}
	return false, false
}

// isPlainBool reports whether t is bool itself; results of a named bool type
// must keep that type, which the schema expression cannot guarantee.
func isPlainBool(t types.Type) bool {
	b, ok := types.Unalias(t).(*types.Basic)
	return ok && (b.Kind() == types.Bool || b.Kind() == types.UntypedBool)
}

func isOrdered(t types.Type) bool {
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsOrdered != 0
}

var errorType = types.Universe.Lookup("error").Type()

func isErrorType(t types.Type) bool {
	return t != nil && types.Identical(t, errorType)
}
