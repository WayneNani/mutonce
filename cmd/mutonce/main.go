// Command mutonce is mutation testing for Go with mutant schemata: all
// mutants are compiled into the code once, behind runtime switches, and each
// mutant only costs a process start of an already built test binary plus the
// tests that actually reach it.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/WayneNani/mutonce/internal/mutant"
	"github.com/WayneNani/mutonce/internal/run"
)

func main() {
	dir := flag.String("C", ".", "directory of the Go module to test")
	mode := flag.String("mode", "schema", "schema, or naive (for comparison: one build per mutant)")
	ops := flag.String("ops", "all", "operators, comma-separated: ROR,LCR,AOR,SBR,RET,ERR,SEL; none for MC/DC alone")
	mcdc := flag.Bool("mcdc", false, "also measure MC/DC coverage of the mutated packages")
	workers := flag.Int("j", runtime.NumCPU(), "parallel test processes")
	factor := flag.Float64("timeout-factor", 5, "timeout = factor × runtime of the tests without mutation + 2s")
	recheck := flag.Bool("recheck", false, "run every mutant twice and mark changing verdicts as flaky")
	reorder := flag.Bool("reorder", false, "run the test that last killed a mutant in the same function first")
	memFactor := flag.Float64("mem-factor", 4, "memory limit = factor × peak memory of the tests without mutation (at least 512 MiB)")
	memBudget := flag.String("mem-budget", "", "memory for all test processes together, e.g. 8G (default: half the available RAM)")
	limit := flag.Int("limit", 0, "run only an even sample of N covered mutants")
	diff := flag.String("diff", "", "only mutate lines changed since this git ref, e.g. main or HEAD~1")
	tests := flag.String("tests", "", "packages whose tests run, comma-separated, e.g. ./... (default: the mutated packages)")
	jsonOut := flag.String("json", "", "also write the result as JSON to this file")
	keep := flag.Bool("keep", false, "keep the work directory with overlay and binaries")
	sandbox := flag.Bool("sandbox", true, "run tests in copies of the module so mutants leave no files in the project")
	verbose := flag.Bool("v", false, "show progress, plus uncovered mutants and timeouts")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mutonce [flags] [packages]   (default: ./...)\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *mode != "schema" && *mode != "naive" {
		fail(fmt.Errorf("unknown mode %q", *mode))
	}
	opSet, err := mutant.ParseOps(*ops)
	if err != nil {
		fail(err)
	}
	if len(opSet) == 0 && !*mcdc {
		fail(fmt.Errorf("-ops none leaves nothing to do without -mcdc"))
	}
	budget, err := parseSize(*memBudget)
	if err != nil {
		fail(err)
	}
	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fail(err)
	}
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	cfg := run.Config{
		Dir:           absDir,
		Patterns:      patterns,
		Ops:           opSet,
		Mode:          *mode,
		Workers:       *workers,
		TimeoutFactor: *factor,
		MemFactor:     *memFactor,
		MemBudget:     budget,
		Limit:         *limit,
		Diff:          *diff,
		TestPatterns:  splitList(*tests),
		Recheck:       *recheck,
		Reorder:       *reorder,
		MCDC:          *mcdc,
		NoSandbox:     !*sandbox,
		Keep:          *keep,
	}
	if *verbose {
		cfg.Log = os.Stderr
	}

	rep, err := run.Run(cfg)
	if err != nil {
		fail(err)
	}
	rep.Print(os.Stdout, *verbose)
	if *jsonOut != "" {
		if err := rep.WriteJSON(*jsonOut); err != nil {
			fail(err)
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// parseSize parses sizes like 512M or 8G; "" means automatic.
func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	shift := 0
	switch strings.ToUpper(s[len(s)-1:]) {
	case "K":
		shift = 10
	case "M":
		shift = 20
	case "G":
		shift = 30
	}
	if shift > 0 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q (e.g. 512M or 8G)", s)
	}
	return n << shift, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mutonce:", err)
	os.Exit(1)
}
