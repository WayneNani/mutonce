// Package mutant finds mutation points in Go packages and rewrites them into
// a mutant schema: every mutant is compiled into the code behind a runtime
// switch, so a single build of each test binary serves all mutants.
package mutant

import (
	"fmt"
	"go/token"
	"strings"
)

// Op identifies a mutation operator.
type Op string

const (
	ROR Op = "ROR" // relational operator replacement, reduced set
	LCR Op = "LCR" // logical connector replacement, reduced set
	AOR Op = "AOR" // arithmetic operator replacement
	SBR Op = "SBR" // statement (block) removal
	RET Op = "RET" // extreme mutation: function returns zero values or an error
	ERR Op = "ERR" // Go: error swallowed, return …, err → return …, nil
	SEL Op = "SEL" // Go: select without its default clause blocks
)

// AllOps lists the operators in report order.
var AllOps = []Op{ROR, LCR, AOR, SBR, RET, ERR, SEL}

// Categories mark the survivors that say most about a test suite.
const (
	CatFunc  = "function"   // a whole function can be emptied
	CatErr   = "error-path" // error handling that no test exercises
	CatGuard = "guard"      // a guard clause that can be removed or weakened
)

// ParseOps parses a comma separated operator list; "" and "all" select all.
func ParseOps(s string) (map[Op]bool, error) {
	ops := map[Op]bool{}
	if s == "" || s == "all" {
		for _, op := range AllOps {
			ops[op] = true
		}
		return ops, nil
	}
	for _, f := range strings.Split(s, ",") {
		op := Op(strings.ToUpper(strings.TrimSpace(f)))
		known := false
		for _, k := range AllOps {
			known = known || op == k
		}
		if !known {
			return nil, fmt.Errorf("unknown operator %q (allowed: ROR, LCR, AOR, SBR, RET, ERR, SEL)", f)
		}
		ops[op] = true
	}
	return ops, nil
}

// Mutant is a single first-order mutant.
type Mutant struct {
	ID       int    `json:"id"`
	Op       Op     `json:"op"`
	Pkg      string `json:"pkg"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	EndLine  int    `json:"endLine"` // last line of the mutated code
	Func     string `json:"func,omitempty"`
	Category string `json:"category,omitempty"`
	Original string `json:"original"`
	Mutated  string `json:"mutated"`
}

// rorTable holds the sufficient ROR replacements of Kaminski, Ammann and
// Offutt: these three mutants per operator subsume the other four.
var rorTable = map[token.Token][]string{
	token.LSS: {"<=", "!=", "false"},
	token.LEQ: {"<", "==", "true"},
	token.GTR: {">=", "!=", "false"},
	token.GEQ: {">", "==", "true"},
	token.EQL: {"<=", ">=", "false"},
	token.NEQ: {"<", ">", "true"},
}

// rorUnordered is used for == and != on types without an order (pointers,
// interfaces, errors, structs): the negation would be killed by every test
// that reaches it, so only the constant replacements remain.
var rorUnordered = []string{"true", "false"}

// lcrTable holds the non-redundant LCR replacements of Just et al.;
// "a" and "b" stand for the left and right operand.
var lcrTable = map[token.Token][]string{
	token.LAND: {"a", "b", "==", "false"},
	token.LOR:  {"a", "b", "!=", "true"},
}

var aorTable = map[token.Token]token.Token{
	token.ADD: token.SUB,
	token.SUB: token.ADD,
	token.MUL: token.QUO,
	token.QUO: token.MUL,
	token.REM: token.MUL,
}

var opTokens = map[string]token.Token{
	"<": token.LSS, "<=": token.LEQ, ">": token.GTR, ">=": token.GEQ,
	"==": token.EQL, "!=": token.NEQ,
}
