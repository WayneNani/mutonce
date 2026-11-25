package mutant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func generateExample(t *testing.T, pattern string) *Result {
	t.Helper()
	dir, err := filepath.Abs("../../example")
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := ParseOps("all")
	res, err := Generate(dir, []string{pattern}, Options{Ops: ops, MCDC: true}, t.Logf)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if os.Getenv("MUTONCE_DUMP") != "" {
		for path, src := range res.Overlay {
			t.Logf("=== %s\n%s", path, src)
		}
	}
	return res
}

func byFunc(ms []Mutant, fn string) []Mutant {
	var out []Mutant
	for _, m := range ms {
		if m.Func == fn {
			out = append(out, m)
		}
	}
	return out
}

func TestGenerateTricky(t *testing.T) {
	res := generateExample(t, "./tricky")

	for _, m := range res.Mutants {
		if m.Line == 13 { // const Limit = 3 * 4
			t.Errorf("constant expression mutated: %+v", m)
		}
	}
	for _, fn := range []string{"Both", "Less"} {
		for _, m := range byFunc(res.Mutants, fn) {
			if m.Op != RET {
				t.Errorf("%s: named bool expression mutated: %+v", fn, m)
			}
		}
	}

	var ror, lcr int
	for _, m := range byFunc(res.Mutants, "Positive") {
		switch {
		case m.Op == ROR && m.Original == "n != nil":
			ror++
			if m.Mutated != "true" && m.Mutated != "false" {
				t.Errorf("unordered comparison got %q", m.Mutated)
			}
		case m.Op == LCR:
			lcr++
		}
	}
	if ror != 2 || lcr != 4 {
		t.Errorf("Positive: %d ROR on n != nil (want 2), %d LCR (want 4)", ror, lcr)
	}

	// Comparing with an enum-like constant is categorical: no ordering variants.
	var enum int
	for _, m := range byFunc(res.Mutants, "Locked") {
		if m.Op == ROR {
			enum++
			if m.Mutated != "true" && m.Mutated != "false" {
				t.Errorf("ordering variant against named constant: %+v", m)
			}
		}
	}
	if enum != 2 {
		t.Errorf("Locked: %d ROR mutants, want 2", enum)
	}

	// Equivalent variants are not generated.
	for _, m := range byFunc(res.Mutants, "(Buffer).Close") {
		if m.Mutated != "returns an error" {
			t.Errorf("Close: unexpected %+v", m)
		}
	}
	for _, m := range byFunc(res.Mutants, "Known") {
		if m.Op == LCR && strings.Contains(m.Mutated, " != ") {
			t.Errorf("Known: equivalent LCR variant %+v", m)
		}
	}

	// The returns inside Sign's terminating if/else must have been filtered.
	var signExcluded int
	for _, m := range res.Excluded {
		if m.Func == "Sign" && m.Op == SBR {
			signExcluded++
		}
	}
	if signExcluded != 3 {
		t.Errorf("Sign: %d SBR mutants excluded, want 3; excluded: %+v", signExcluded, res.Excluded)
	}
	if res.Rounds < 2 {
		t.Errorf("expected a filter round, got %d rounds", res.Rounds)
	}

	host := res.Runtime["example.com/shop/tricky"]
	if !strings.Contains(string(res.Overlay[host]), schemaActiveDecl) {
		t.Errorf("runtime helpers missing in host file %s", host)
	}
	if naive := WithActive(res.Overlay[host], 7); !strings.Contains(string(naive), "const _moActive = 7") {
		t.Errorf("WithActive did not hard-code the mutant")
	}
}

func TestGenerateAll(t *testing.T) {
	res := generateExample(t, "./...")
	seen := map[int]bool{}
	for _, m := range append(res.Mutants, res.Excluded...) {
		if seen[m.ID] {
			t.Fatalf("duplicate id %d", m.ID)
		}
		seen[m.ID] = true
	}
	for i := 1; i <= len(seen); i++ {
		if !seen[i] {
			t.Fatalf("ids not dense: %d missing", i)
		}
	}
	cats := map[string]int{}
	for _, m := range res.Mutants {
		if m.EndLine < m.Line {
			t.Errorf("end line before start: %+v", m)
		}
		if m.Op == ERR || m.Op == SEL {
			cats[string(m.Op)+":"+m.Func]++
		}
		if m.Func == "Final" && m.Original == "err != nil" && m.Category != CatErr {
			t.Errorf("error check not categorised: %+v", m)
		}
		// if v < lo { return lo } is a guard: removing or weakening it.
		if m.Func == "Clamp" && (m.Op == SBR && strings.HasPrefix(m.Original, "if v < lo") || m.Op == ROR && m.Original == "v < lo") && m.Category != CatGuard {
			t.Errorf("guard not categorised: %+v", m)
		}
		// A comparison in a return statement is no guard.
		if m.Func == "Positive" && m.Category == CatGuard {
			t.Errorf("non-guard categorised as guard: %+v", m)
		}
		if m.Func == "Total" && strings.Contains(m.Original, "slog.Debug") {
			t.Errorf("arid logging call mutated: %+v", m)
		}
		if m.Original == "len(items) == 0" && m.Mutated == "len(items) <= 0" {
			t.Errorf("trivially equivalent mutant generated: %+v", m)
		}
	}
	// `return 0, ErrEmpty` and `return 0, err`, but not `return sum, nil`.
	if cats["ERR:Total"] != 1 || cats["ERR:Final"] != 1 || cats["SEL:TryNotify"] != 1 {
		t.Errorf("Go-specific mutants: %v", cats)
	}
}
