package mutant

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
)

// PkgInfo describes a package matched by the patterns.
type PkgInfo struct {
	Path     string
	Name     string
	Dir      string
	HasTests bool
}

// Options select what Generate builds into the schema.
type Options struct {
	Ops  map[Op]bool
	MCDC bool // instrument decisions for MC/DC coverage
	// Selected, if not nil, picks the mutants and decisions to build in
	// (diff mode); IDs stay the same either way.
	Selected func(Mutant) bool
}

// Result is a type-correct mutant schema.
type Result struct {
	Mutants  []Mutant
	Excluded []Mutant // dropped because the schema would not compile
	// Decisions are instrumented for MC/DC; ExcludedDecisions counts those
	// whose probes would not compile.
	Decisions         []Decision
	ExcludedDecisions int
	// Unselected counts the mutants left out by the selection (diff mode).
	Unselected int
	Overlay    map[string][]byte
	Runtime    map[string]string // package path -> overlay file holding the helpers
	Pkgs       []PkgInfo
	Rounds     int
}

const maxRounds = 20

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax

// Generate builds the schema for the packages matched by patterns in dir.
// Mutants whose code does not type-check are dropped and the schema is
// regenerated until it compiles, since one bad mutant would break the build
// of all others.
func Generate(dir string, patterns []string, opts Options, logf func(string, ...any)) (*Result, error) {
	excluded, excludedDec := map[int]bool{}, map[int]bool{}
	for round := 1; round <= maxRounds; round++ {
		pkgs, err := load(dir, patterns)
		if err != nil {
			return nil, err
		}
		res, errs, err := generate(pkgs, opts, excluded, excludedDec)
		if err != nil {
			return nil, err
		}
		res.Rounds = round
		if len(errs) == 0 {
			return res, nil
		}
		bad, badDec, err := attribute(errs, res.Overlay)
		if err != nil {
			return nil, err
		}
		progress := false
		for id := range bad {
			progress = progress || !excluded[id]
			excluded[id] = true
		}
		for id := range badDec {
			progress = progress || !excludedDec[id]
			excludedDec[id] = true
		}
		if !progress {
			return nil, fmt.Errorf("schema does not compile: %s: %s", errs[0].File, errs[0].Msg)
		}
		logf("round %d: %d type errors, %d mutants and %d decisions dropped", round, len(errs), len(bad), len(badDec))
	}
	return nil, fmt.Errorf("schema still broken after %d rounds", maxRounds)
}

func load(dir string, patterns []string) ([]*packages.Package, error) {
	cfg := &packages.Config{Mode: loadMode, Dir: dir}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].PkgPath < pkgs[j].PkgPath })
	return pkgs, nil
}

func generate(pkgs []*packages.Package, opts Options, excluded, excludedDec map[int]bool) (*Result, []typeError, error) {
	res := &Result{Overlay: map[string][]byte{}, Runtime: map[string]string{}}
	// drop holds the excluded mutants plus those not selected; neither is
	// written into the schema. dropDec does the same for decisions.
	drop, dropDec := maps.Clone(excluded), maps.Clone(excludedDec)
	selected := opts.Selected
	var errs []typeError
	next, nextDec := 1, 1
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			return nil, nil, fmt.Errorf("cannot load package %s: %v", pkg.PkgPath, pkg.Errors[0])
		}
		if len(pkg.GoFiles) == 0 {
			continue
		}
		dir := filepath.Dir(pkg.GoFiles[0])
		res.Pkgs = append(res.Pkgs, PkgInfo{Path: pkg.PkgPath, Name: pkg.Name, Dir: dir, HasTests: hasTestFiles(dir)})
		if usesCgo(pkg) {
			continue
		}
		host := ""
		for _, file := range pkg.Syntax {
			if ast.IsGenerated(file) {
				continue
			}
			filename := pkg.Fset.File(file.Pos()).Name()
			src, err := os.ReadFile(filename)
			if err != nil {
				return nil, nil, err
			}
			g := newFileGen(pkg, file, src, opts, &next, &nextDec)
			g.collect()
			for _, m := range g.mutants {
				switch {
				case selected != nil && !selected(m):
					drop[m.ID] = true
					res.Unselected++
				case excluded[m.ID]:
					res.Excluded = append(res.Excluded, m)
				default:
					res.Mutants = append(res.Mutants, m)
				}
			}
			for _, d := range g.decisions {
				switch {
				case selected != nil && !selected(Mutant{File: d.File, Line: d.Line, EndLine: d.EndLine}):
					dropDec[d.ID] = true
				case excludedDec[d.ID]:
					res.ExcludedDecisions++
				default:
					res.Decisions = append(res.Decisions, d)
				}
			}
			if !g.rewrite(drop, dropDec) {
				continue
			}
			file.Comments = directives(file.Comments)
			if host == "" {
				for _, imp := range runtimeImports {
					astutil.AddNamedImport(pkg.Fset, file, imp[0], imp[1])
				}
			}
			var buf bytes.Buffer
			if err := format.Node(&buf, pkg.Fset, file); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", filename, err)
			}
			if host == "" {
				host = filename
				buf.WriteString(runtimeBody)
			}
			res.Overlay[filename] = buf.Bytes()
		}
		if host != "" {
			res.Runtime[pkg.PkgPath] = host
			pkgErrs, err := typecheck(pkg, res.Overlay)
			if err != nil {
				return nil, nil, err
			}
			errs = append(errs, pkgErrs...)
		}
	}
	return res, errs, nil
}

// directives keeps only comment groups that matter to the toolchain
// (//go:embed, //go:build, //line, ...). Other comments would be scattered
// through the rewritten code by the printer, since new nodes have no position.
func directives(groups []*ast.CommentGroup) []*ast.CommentGroup {
	var keep []*ast.CommentGroup
	for _, g := range groups {
		for _, c := range g.List {
			if strings.HasPrefix(c.Text, "//go:") || strings.HasPrefix(c.Text, "//line ") {
				keep = append(keep, g)
				break
			}
		}
	}
	return keep
}

func hasTestFiles(dir string) bool {
	m, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
	return len(m) > 0
}

// usesCgo reports whether the compiled files differ from the source files,
// which is the case for cgo packages; those are left alone.
func usesCgo(pkg *packages.Package) bool {
	src := map[string]bool{}
	for _, f := range pkg.GoFiles {
		src[f] = true
	}
	for _, f := range pkg.CompiledGoFiles {
		if !src[f] {
			return true
		}
	}
	return false
}

type typeError struct {
	File   string
	Offset int
	Msg    string
}

// stdImporter supplies the packages only the runtime file imports.
var stdImporter = importer.Default()

// typecheck checks the schema of one package in-process. Mutation adds only
// unexported identifiers, so the exported API of every package is unchanged
// and the imports can be taken from the original load.
func typecheck(pkg *packages.Package, overlay map[string][]byte) ([]typeError, error) {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range pkg.CompiledGoFiles {
		src, ok := overlay[name]
		if !ok {
			var err error
			if src, err = os.ReadFile(name); err != nil {
				return nil, err
			}
		}
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("schema for %s is not valid Go: %w", pkg.PkgPath, err)
		}
		files = append(files, f)
	}
	imports := map[string]*types.Package{}
	for _, p := range pkg.Types.Imports() {
		imports[p.Path()] = p
	}
	var errs []typeError
	conf := types.Config{
		Importer: importerFunc(func(path string) (*types.Package, error) {
			if p, ok := imports[path]; ok {
				return p, nil
			}
			if p, ok := pkg.Imports[path]; ok && imports[p.ID] != nil {
				return imports[p.ID], nil
			}
			return stdImporter.Import(path)
		}),
		Sizes: types.SizesFor("gc", runtime.GOARCH),
		Error: func(err error) {
			if te, ok := err.(types.Error); ok {
				pos := fset.Position(te.Pos)
				errs = append(errs, typeError{File: pos.Filename, Offset: pos.Offset, Msg: te.Msg})
			}
		},
	}
	conf.Check(pkg.PkgPath, fset, files, nil)
	return errs, nil
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// attribute maps type errors in the generated files back to the mutants
// and decisions that caused them: the guards and probes in the innermost
// node around the error.
func attribute(errs []typeError, overlay map[string][]byte) (bad, badDec map[int]bool, err error) {
	bad, badDec = map[int]bool{}, map[int]bool{}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, e := range errs {
		src, ok := overlay[e.File]
		if !ok {
			return nil, nil, fmt.Errorf("unexpected error outside the schema: %s: %s", e.File, e.Msg)
		}
		f := parsed[e.File]
		if f == nil {
			if f, err = parser.ParseFile(fset, e.File, src, parser.SkipObjectResolution); err != nil {
				return nil, nil, err
			}
			parsed[e.File] = f
		}
		pos := fset.File(f.Pos()).Pos(e.Offset)
		path, _ := astutil.PathEnclosingInterval(f, pos, pos)
		ids, decs := map[int]bool{}, map[int]bool{}
		// go/types reports a missing return at the closing brace; the culprit
		// is a removed statement at the end of the function.
		if strings.Contains(e.Msg, "missing return") && len(path) > 0 {
			if b, ok := path[0].(*ast.BlockStmt); ok && len(b.List) > 0 {
				terminalWrappers(b.List[len(b.List)-1], ids)
			}
		}
		for _, n := range path {
			if len(ids)+len(decs) > 0 {
				break
			}
			collectGuards(n, ids, decs)
		}
		if len(ids)+len(decs) == 0 {
			return nil, nil, fmt.Errorf("type error not attributable to a mutant: %s: %s", e.File, e.Msg)
		}
		maps.Copy(bad, ids)
		maps.Copy(badDec, decs)
	}
	return bad, badDec, nil
}

// terminalWrappers collects SBR wrappers that end a statement list, i.e.
// those that may have destroyed a terminating statement.
func terminalWrappers(s ast.Stmt, ids map[int]bool) {
	if id, inner, ok := sbrWrapper(s); ok {
		ids[id] = true
		terminalWrappers(inner, ids)
		return
	}
	lastOf := func(list []ast.Stmt) {
		if len(list) > 0 {
			terminalWrappers(list[len(list)-1], ids)
		}
	}
	clauses := func(body *ast.BlockStmt) {
		for _, c := range body.List {
			switch c := c.(type) {
			case *ast.CaseClause:
				lastOf(c.Body)
			case *ast.CommClause:
				lastOf(c.Body)
			}
		}
	}
	switch s := s.(type) {
	case *ast.BlockStmt:
		lastOf(s.List)
	case *ast.IfStmt:
		lastOf(s.Body.List)
		if s.Else != nil {
			terminalWrappers(s.Else, ids)
		}
	case *ast.LabeledStmt:
		terminalWrappers(s.Stmt, ids)
	case *ast.SwitchStmt:
		clauses(s.Body)
	case *ast.TypeSwitchStmt:
		clauses(s.Body)
	case *ast.SelectStmt:
		clauses(s.Body)
	}
}

// sbrWrapper recognises `if !_moIs(id) { stmt }`.
func sbrWrapper(s ast.Stmt) (int, ast.Stmt, bool) {
	ifs, ok := s.(*ast.IfStmt)
	if !ok || ifs.Init != nil || ifs.Else != nil || len(ifs.Body.List) != 1 {
		return 0, nil, false
	}
	not, ok := ifs.Cond.(*ast.UnaryExpr)
	if !ok || not.Op != token.NOT {
		return 0, nil, false
	}
	id, n, ok := guardCall(not.X)
	if !ok || n != 1 {
		return 0, nil, false
	}
	return id, ifs.Body.List[0], true
}

func collectGuards(n ast.Node, ids, decs map[int]bool) {
	ast.Inspect(n, func(n ast.Node) bool {
		if id, cnt, ok := guardCall(n); ok {
			for i := range cnt {
				ids[id+i] = true
			}
		}
		if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 3 {
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "_moD" {
				if id, ok := intArg(call.Args[0]); ok {
					decs[id] = true
				}
			}
		}
		return true
	})
}

func guardCall(n ast.Node) (id, count int, ok bool) {
	call, isCall := n.(*ast.CallExpr)
	if !isCall {
		return 0, 0, false
	}
	fn, isIdent := call.Fun.(*ast.Ident)
	if !isIdent {
		return 0, 0, false
	}
	switch {
	case fn.Name == "_moIs" && len(call.Args) == 1, fn.Name == "_moNoErr" && len(call.Args) == 2:
		id, ok = intArg(call.Args[0])
		return id, 1, ok
	case fn.Name == "_moOff" && len(call.Args) == 2:
		id, ok1 := intArg(call.Args[0])
		count, ok2 := intArg(call.Args[1])
		return id, count, ok1 && ok2
	}
	return 0, 0, false
}

func intArg(e ast.Expr) (int, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	v, err := strconv.Atoi(lit.Value)
	return v, err == nil
}
