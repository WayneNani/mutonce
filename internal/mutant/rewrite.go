package mutant

import (
	"go/ast"
	"go/token"
	"slices"
	"strconv"

	"golang.org/x/tools/go/ast/astutil"
)

// rewrite turns the planned sites into schema code. Children are rewritten
// before their parents, so nested mutation points end up inside the
// original as well as inside every mutated variant of the enclosing site.
func (g *fileGen) rewrite(excluded map[int]bool) bool {
	changed := false
	astutil.Apply(g.file, nil, func(c *astutil.Cursor) bool {
		orig := c.Node()
		node := orig
		// SBR goes last, so it wraps whatever the other sites made of the node.
		sites := slices.Clone(g.plan[orig])
		slices.SortStableFunc(sites, func(a, b *site) int { return cmpBool(a.op == SBR, b.op == SBR) })
		for _, s := range sites {
			if s.allExcluded(excluded) {
				continue
			}
			changed = true
			switch s.op {
			case ROR, LCR:
				node = boolSchema(node.(*ast.BinaryExpr), s, excluded)
			case AOR:
				node = aorSchema(node.(*ast.BinaryExpr), s)
			case ERR:
				// _moNoErr(id, err) yields nil while the mutant is active.
				node = &ast.CallExpr{Fun: ast.NewIdent("_moNoErr"), Args: []ast.Expr{intLit(s.ids[0]), node.(ast.Expr)}}
			case SEL:
				node = selSchema(node.(*ast.SelectStmt), s)
			case SBR:
				// if !_moIs(id) { stmt }
				node = &ast.IfStmt{
					Cond: &ast.UnaryExpr{Op: token.NOT, X: guard("_moIs", s.ids[0])},
					Body: &ast.BlockStmt{List: []ast.Stmt{node.(ast.Stmt)}},
				}
			case RET:
				retSchema(node.(*ast.FuncDecl), s, excluded)
			}
		}
		if node != orig {
			c.Replace(node)
		}
		return true
	})
	return changed
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// selSchema turns a select with a default clause into
//
//	if _moIs(id) { select { …without default… } } else { select { … } }
//
// A clause cannot be switched off at run time, so the select is duplicated.
func selSchema(sel *ast.SelectStmt, s *site) ast.Stmt {
	var clauses []ast.Stmt
	for _, c := range sel.Body.List {
		if cc, ok := c.(*ast.CommClause); !ok || cc.Comm != nil {
			clauses = append(clauses, c)
		}
	}
	return &ast.IfStmt{
		Cond: guard("_moIs", s.ids[0]),
		Body: &ast.BlockStmt{List: []ast.Stmt{&ast.SelectStmt{Body: &ast.BlockStmt{List: clauses}}}},
		Else: &ast.BlockStmt{List: []ast.Stmt{sel}},
	}
}

// boolSchema turns `x op y` into
//
//	(_moOff(base, n) && (x op y)) || (_moIs(base) && (variant0)) || ...
//
// Go has no conditional expression, but for booleans this form needs no
// closure: the original comes first, keeps its short-circuit evaluation,
// and no variant is evaluated unless its mutant is active.
func boolSchema(be *ast.BinaryExpr, s *site, excluded map[int]bool) ast.Expr {
	expr := ast.Expr(&ast.BinaryExpr{X: guard("_moOff", s.ids[0], len(s.ids)), Op: token.LAND, Y: paren(be)})
	for i, v := range s.vars {
		id := s.ids[i]
		if excluded[id] || v == "false" {
			continue // a false term never contributes; _moOff already yields false
		}
		var term ast.Expr = guard("_moIs", id)
		switch v {
		case "true":
		case "a":
			term = and(term, paren(be.X))
		case "b":
			term = and(term, paren(be.Y))
		default:
			term = and(term, paren(&ast.BinaryExpr{X: be.X, Op: opTokens[v], Y: be.Y}))
		}
		expr = &ast.BinaryExpr{X: expr, Op: token.LOR, Y: paren(term)}
	}
	return paren(expr)
}

// aorSchema turns `x op y` into
//
//	func() T { if _moIs(id) { return x mop y }; return x op y }()
//
// For non-boolean values a closure is the only way to select a variant
// without evaluating both of them.
func aorSchema(be *ast.BinaryExpr, s *site) ast.Expr {
	mut := &ast.BinaryExpr{X: be.X, Op: aorTable[be.Op], Y: be.Y}
	return &ast.CallExpr{Fun: &ast.FuncLit{
		Type: &ast.FuncType{
			Params:  &ast.FieldList{},
			Results: &ast.FieldList{List: []*ast.Field{{Type: ast.NewIdent(s.typ)}}},
		},
		Body: &ast.BlockStmt{List: []ast.Stmt{
			&ast.IfStmt{
				Cond: guard("_moIs", s.ids[0]),
				Body: &ast.BlockStmt{List: []ast.Stmt{&ast.ReturnStmt{Results: []ast.Expr{mut}}}},
			},
			&ast.ReturnStmt{Results: []ast.Expr{be}},
		}},
	}}
}

// retSchema prepends `if _moIs(id) { return *new(T1), ..., _moErr }` guards.
// Reusing the declared result type expressions keeps them valid in scope,
// including type parameters.
func retSchema(fd *ast.FuncDecl, s *site, excluded map[int]bool) {
	var guards []ast.Stmt
	for i, v := range s.vars {
		id := s.ids[i]
		if excluded[id] {
			continue
		}
		var results []ast.Expr
		if fd.Type.Results != nil {
			for _, f := range fd.Type.Results.List {
				for range max(1, len(f.Names)) {
					results = append(results, &ast.StarExpr{X: &ast.CallExpr{
						Fun:  ast.NewIdent("new"),
						Args: []ast.Expr{f.Type},
					}})
				}
			}
		}
		if v == "err" {
			results[len(results)-1] = ast.NewIdent("_moErr")
		}
		guards = append(guards, &ast.IfStmt{
			Cond: guard("_moIs", id),
			Body: &ast.BlockStmt{List: []ast.Stmt{&ast.ReturnStmt{Results: results}}},
		})
	}
	fd.Body.List = append(guards, fd.Body.List...)
}

func guard(fn string, args ...int) *ast.CallExpr {
	call := &ast.CallExpr{Fun: ast.NewIdent(fn)}
	for _, a := range args {
		call.Args = append(call.Args, intLit(a))
	}
	return call
}

func intLit(i int) *ast.BasicLit {
	return &ast.BasicLit{Kind: token.INT, Value: strconv.Itoa(i)}
}

func paren(e ast.Expr) ast.Expr { return &ast.ParenExpr{X: e} }

func and(a, b ast.Expr) ast.Expr { return &ast.BinaryExpr{X: a, Op: token.LAND, Y: b} }
