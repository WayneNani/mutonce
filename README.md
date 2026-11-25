# mutonce

**Mutation testing for Go that builds once.**

Mutation testing changes your code in small ways — a `<` becomes `<=`, a
statement disappears, a function returns zero values — and checks whether your
tests notice. Mutants that survive point at code that is executed by tests but
not actually checked by them.

Existing Go tools build and link a fresh test binary for every mutant, which
takes hours on larger modules. mutonce uses **mutant schemata** instead: all
mutants are compiled into the code at once, each behind a runtime switch. Every
test binary is built once; a mutant then only costs starting that binary with
an environment variable, and only the tests that actually reach the mutated
code run.

With `-mcdc`, the same run also measures **MC/DC coverage** (modified
condition/decision coverage): for every condition in an `&&`/`||` expression,
whether some test shows that it changes the outcome on its own. Where none
does, the report names the missing test case.

> **Status:** early prototype. It works on real-world modules (see
> [Results](#results)), but the command line and report format may still change.

## Installation

```bash
go install github.com/WayneNani/mutonce/cmd/mutonce@latest
```

mutonce needs Go 1.26 or newer to build. It type-checks the module under test
with the `go/types` of the toolchain it was built with, so build it with a
toolchain at least as new as the Go version in your module's `go.mod`, for
example `GOTOOLCHAIN=go1.27.3 go install …`.

## Quick start

```bash
cd your/module
mutonce                       # mutate ./... and run the tests
mutonce -diff main            # only code changed since main (for pull requests)
mutonce -tests ./... ./internal/server   # mutate one package, use all tests
mutonce -json report.json -v  # full details
mutonce -mcdc                 # mutation testing plus MC/DC coverage
mutonce -mcdc -ops none       # MC/DC coverage only, in seconds
```

A shortened report for the [example module](example) in this repository:

```text
Mutants:   162 in the schema, 5 dropped by the type check (2 round(s))
Tests:     19 in 3 test binaries
Side effects: test runs created 1 file(s), removed again in the sandbox: tricky/.tmp

  Operator  Total  Killed  Timeout  Memory  Survived  Uncovered  Score (covered)
       ROR     62      38        1       1        19          3           67.8 %
       ...
     Total    162     115        3       1        29         14           80.4 %

Bypassable checks (14 in 5 function(s))
  A condition that exits early (return, continue, panic) can be removed or
  weakened without any test failing.
  Clamp  internal/mathx/mathx.go
    line 9   SBR  #2   statement removed: if v < lo { …
    line 9   ROR  #4   v < lo  →  v <= lo
    ...
```

### Flags

| Flag | Meaning |
|---|---|
| `-diff <ref>` | only mutate code touching lines changed since a git ref, including uncommitted and untracked files |
| `-tests <patterns>` | packages whose tests run, independent of the mutated packages (default: the mutated packages) |
| `-ops ROR,LCR,AOR,SBR,RET,ERR,SEL` | operators to use (default: all); `none` together with `-mcdc` measures MC/DC only |
| `-mcdc` | also measure MC/DC coverage of the mutated packages (see [MC/DC coverage](#mcdc-coverage)) |
| `-recheck` | run every mutant twice; changing verdicts are reported as flaky and excluded from the score |
| `-j N` | parallel test processes (default: number of CPUs) |
| `-timeout-factor F` | timeout = F × runtime of the tests without mutation + 2 s (default 5) |
| `-mem-factor F`, `-mem-budget 8G` | memory limit per process and for all processes together (see below) |
| `-limit N` | run an even sample of N covered mutants |
| `-sandbox=false` | run tests in the module itself instead of sandbox copies (not recommended) |
| `-mode naive` | build one binary per mutant, for comparison |
| `-json file`, `-v`, `-keep` | JSON output, all details, keep the work directory |
| `-reorder` | run the last killing test of a function first (experimental; no measurable gain so far) |

## Platform support

| Platform | Process isolation | Memory limit |
|---|---|---|
| Linux | own process group | hard, `RLIMIT_DATA` |
| macOS, FreeBSD, OpenBSD, NetBSD | own process group | resident memory watched, process group killed above the limit |
| Windows | job object (kills the whole process tree) | hard, job memory limit |

On macOS and the BSDs a mutant that allocates memory that compresses well may
reach its timeout before its resident memory crosses the limit; it is stopped
either way. On other platforms only the timeout and the shared memory budget
apply. CI runs
the test suite on Linux, macOS and Windows.

## How it works

1. **Generate the schema** ([internal/mutant](internal/mutant)). Packages are
   loaded with `go/packages`, mutation points are chosen with `go/types`, and
   the AST is rewritten. The source tree is never touched: rewritten files are
   passed to the build with `go build -overlay`.
2. **Type-check filter.** A single mutant that does not compile would break the
   build of all others. The schema is type-checked, errors are mapped back to
   the mutants that caused them, those are dropped, and the schema is
   regenerated until it compiles. A typical case is a removed `return` at the
   end of an `if`/`else` chain (*missing return*).
3. **Build once.** One `go test -c -overlay=…` per package with tests. Each
   binary contains the schema of every mutated package it depends on, so
   mutants in packages without tests of their own are killed by the tests of
   the packages that use them.
4. **Per-test coverage.** Every test runs once on its own with recording
   enabled, and the runtime helpers note every mutation point reached. That is
   one run per test rather than per test and mutant, and it yields the per-test
   coverage that `go test -cover` cannot provide.
5. **Run mutants.** For each mutant, only the tests that reach it run, with
   `MUTONCE_ACTIVE=<id>` and `-test.failfast`. The timeout catches mutants that
   loop forever.
6. **Memory limit.** The counterpart of the timeout: a mutant may use at most
   four times the peak memory of its tests without mutation (at least
   512 MiB), and every process reserves its limit from a shared budget (default:
   half the available memory) before it starts. Exceeding it counts as
   detected. This is not optional — a mutant that removes a loop's exit
   condition allocates gigabytes within seconds, and the kernel's OOM killer
   may then take down whatever shares the session with the test process.
7. **Sandboxes.** Mutants change behaviour, and some behaviour writes files: a
   path mutated to `""` turns `path + ".tmp"` into `.tmp` in the working
   directory. Each parallel worker therefore gets its own copy of the module
   (without `.git`; `node_modules` and files over 1 MiB are linked). Tests run
   in the package directory of that copy, and anything a run creates there is
   removed again and reported as a side effect. As a bonus, runs are isolated
   from each other.
8. **Report.** Survivors are ordered by what they say about the tests and
   grouped by function: functions that can be emptied without any test failing,
   bypassable checks (guard clauses whose removal nobody notices), untested
   error handling, then the rest.

### What the schema looks like

Go has no conditional expression. For boolean expressions the schema still
needs no closure, and the original keeps its short-circuit evaluation:

```go
// n != nil && n.Val > 0   (LCR; the operands contain ROR mutants themselves)
(_moOff(10, 4) && (orig)) || (_moIs(10) && (a)) || (_moIs(11) && (b)) || (_moIs(12) && (a == b))
```

Arithmetic needs a closure, otherwise both variants would be evaluated (for
example a division by zero on the original path):

```go
func() int { if _moIs(7) { return n / 2 }; return n * 2 }()
```

Statements become `if !_moIs(id) { stmt }`. Functions get
`if _moIs(id) { return *new(T1), …, _moErr }` in front; `*new(T)` reuses the
declared result type and therefore works with type parameters. A swallowed
error becomes `return x, _moNoErr(id, err)`. A `select` clause cannot be
switched off at run time, so for SEL the `select` is duplicated. The helpers
are unexported and appended to the first mutated file of each package
([runtime.go](internal/mutant/runtime.go)).

## Operators

The selection follows the mutation testing literature:

| Operator | What it does | Basis |
|---|---|---|
| ROR | reduced relational operator replacement: 3 instead of 7 variants; `==`/`!=` on unordered types (pointers, `error`, interfaces) only `true`/`false` | subsumption (Kaminski, Ammann, Offutt); most productive operator at Google |
| LCR | `a && b` → `a`, `b`, `a == b`, `false`; `a \|\| b` → `a`, `b`, `a != b`, `true` | non-redundant set (Just et al.) |
| AOR | `+`↔`-`, `*`↔`/`, `%`→`*`, numeric types only | part of the 5-selective set; less productive at Google, so one variant only |
| SBR | remove a statement or block, including `return err` in an error branch | 68 % of all mutants at Google, few equivalents |
| RET | a function returns zero values, or additionally an injected error | extreme mutation (Descartes); finds "pseudo-tested" functions |
| ERR | `return …, err` → `return …, nil`: the error is swallowed | Go-specific |
| SEL | a `select` without its `default` clause, so it blocks | Go-specific; message passing causes a disproportionate share of blocking bugs (Tu et al.) |

Removing `defer`, `close(ch)`, `mu.Unlock()` or `wg.Done()` is already covered by SBR.

Not mutated:

- **Arid nodes** (after Google): logging (`log`, `log/slog`, `fmt.Print*`, zap,
  logrus, zerolog), Prometheus calls, `time.Sleep`, the capacity argument of
  `make`.
- **Constant expressions**, and expressions of a named boolean type.
- **Trivially equivalent variants**, such as `len(s) == 0` → `len(s) <= 0`, or
  `u != 0` → `u > 0` for unsigned integers and strings.
- **Ordering variants of comparisons with named constants.** Go has no enum
  type, and `iota` constants are ordered numbers to the type checker, so
  `status != Locked` only gets `true`/`false`, not `status > Locked`. On a real
  project such variants were almost a fifth of the surviving ROR mutants.
- **Zero values where there already are zero values:** `func (f *File) Close()
  error { return nil }` only gets the "returns an error" variant.
- **`!=` for mutually exclusive equalities:** in `x == A || x == B` with
  different constants both sides cannot hold, so `!=` equals `||`.

UOI and ABS are left out (rarely productive according to Google).

## MC/DC coverage

MC/DC is the coverage criterion of avionics (DO-178C level A) and automotive
safety standards (ISO 26262). A *decision* is a boolean expression with at
least two *conditions* joined by `&&` or `||`; a condition is an operand that
contains no `&&`, `||` or `!` itself. MC/DC asks for every condition to be shown
to affect the outcome on its own: two evaluations in which only that condition
changes, and the outcome changes with it.

```text
MC/DC: 4 of 6 conditions (66.7 %) in 3 decisions

Conditions without a shown independent effect (1 decision(s))
  No pair of test runs changes only this condition and with it the outcome.
  pricing/pricing.go:53  FreeShipping  premium || total > 2500
    premium       both values seen  missing: premium = false, total > 2500 = false → false
    total > 2500  only true seen    missing: premium = false, total > 2500 = false → false
```

Here the tests only ever ask for free shipping; a single test with a small,
non-premium order would cover both conditions.

**How it is measured.** The decisions are instrumented in the same schema as
the mutants, and the vectors are recorded in the per-test coverage run that
happens anyway, so `-mcdc -ops none` costs one build and one run of each test.
Every condition is wrapped in a probe that notes its value, and the decision
in a closure holding the vector of one evaluation:

```go
// premium || total > 2500
func() bool {
	var _mov uint64
	return _moD(1, (_moC(&_mov, 0, premium) || _moC(&_mov, 1, total > 2500)), &_mov)
}()
```

A vector per evaluation keeps recursion and goroutines apart. Go evaluates
`&&` and `||` with short circuit, so a condition that was not evaluated matches
either value when looking for a pair ("unique cause with short circuit", as in
the MC/DC modes of GCC and LLVM). Only passing tests count. Constant operands
(`debug && …`) are not conditions; decisions of a named boolean type and those
calling `recover` are left out, because a closure would change their meaning.

**MC/DC or mutation score?** They overlap: the ROR and LCR mutants of a
decision can only be killed by tests that separate its conditions, so a good
mutation score usually implies most of MC/DC. Mutation testing is the stricter
measure, because a test must also *check* the result, not just cause it. MC/DC
is cheaper, needs no test to fail, and is what a certification asks for.

## Results

AMD Ryzen 7 5800X (8 cores / 16 threads), `-j 16`. "Naive" means one
`go test -c` per mutant with the same test selection, so the difference is the
build work alone.

| Target | Mutants | Schema | Naive | Speed-up | Differing verdicts |
|---|---|---|---|---|---|
| [example](example) | 118 | 2.7 s | 8.6 s | 3× | 0 |
| `golang.org/x/mod/semver` | 492 | 4.1 s | 64.3 s | 15× | 1 (flaky test, see below) |
| `golang.org/x/tools/go/ast/astutil` (sample) | 200 | 0.7 s | 25.1 s | 37× | 0 |

In the example, three deliberate endless loops dominate, each running until its
timeout. Without timeouts a mutant takes 1–6 ms in schema mode and 1–2 s of
build time in naive mode.

The one difference for `semver` (`cmp < 0` → `false` in `ByVersion.Less`) is
not a schema bug: `TestSort` shuffles its input with `rand.Shuffle` and checks
the result with `sort.IsSorted` — that is, with the same mutated `Less`. The
mutant is killed in about half of the runs, which also shows that mutation
analysis exposes weak test oracles.

With `-mcdc`, all 1,679 decisions of `golang.org/x/tools` (every package)
passed the type check and the compiler without a single one being dropped.
For `astutil`, MC/DC came to 39 of 62 conditions (62.9 %); the mutation
verdicts were identical with and without it.

### A real-world module

A closed-source application with 21 packages and about 300 tests
(`go test ./...` takes about 6 s):

| | |
|---|---|
| Mutants in the schema | about 16,400 (3 dropped by the type check) |
| Reached by tests | about 13,000 |
| Schema + type check | 2 s |
| Build test binaries | 70–80 s cold, 1.5 s with a warm build cache |
| Per-test coverage | 4 s |
| Run mutants | 18–21 min (80–95 ms per mutant, 16 processes) |
| Peak memory | 3.1 GB (budget 8 GB) |

A naive run is impractical there: a rebuild after a mutation takes several
seconds, which would mean many hours for 13,000 mutants. With `-diff` against a
three-commit range (15 changed lines) the run took 6 s.

What the runs found, and what the tests written in response showed:

- **26 functions could be emptied** without any test failing. They were called
  by tests, but nobody checked their effect. Several were deletion and
  acknowledgement handlers; writing tests for them uncovered a real bug in a
  client that reported success on HTTP 403 and 404.
- **547 error checks** survived the mutation `err != nil` → `false`: no test
  triggered those error paths.
- **Bypassable checks** (1,550 survivors in 294 functions) contained the most
  valuable findings, such as signature checks whose removal no test noticed.
  The category is large and mixes critical with harmless guards; ranking it by
  kind of check is the next step.
- **Pseudo-tests of bindings** — where signing and verifying use the same
  helper that builds the signed text — show up reliably as "functions without
  an effective test". The tampering test that kills them still has to be
  written by hand.
- About **0.3 % of mutants were flaky** across repeated runs of the same
  packages; `-recheck` exists for reliable individual verdicts.
- The sandboxes caught **three side effects** per run (`.tmp` files that
  would otherwise have been left in the source tree).

## Limitations

- **Isolation:** the memory limit protects against runaway mutants, but builds
  run unrestricted. Sandboxes only catch writes relative to the working
  directory; a test that writes into the source tree through an absolute path
  (for example from `runtime.Caller`) still reaches it. When running mutonce
  from an IDE or desktop app on Linux, a separate scope adds safety:
  `systemd-run --user --scope -p MemoryMax=12G mutonce …`
- **Diff mode** selects mutants whose code touches a changed line. A change
  anywhere in a function selects its RET mutants; unchanged callers of a
  changed function are not mutated. Coverage is still recorded for all tests.
- **Test selection** works on top-level tests; subtests are not selected
  individually.
- **Timing:** the switches prevent some inlining, and closures change escape
  analysis. Benchmarks and allocation tests (`testing.AllocsPerRun`) may give
  wrong results.
- **Type-check filter:** errors only `gc` reports would abort instead of being
  filtered; not observed so far.
- **Module cache:** for packages located directly in the module cache, `go`
  ignores new imports from the overlay. Copy third-party code elsewhere to
  measure it.
- **cgo packages** and generated files are skipped; function literals get no
  RET mutants.
- **MC/DC** covers boolean expressions of the mutated, non-generated files only.
  Decisions with more than 32 conditions are skipped; for more than 12, no
  missing test case is proposed. Like the switches, the probe closures cost a
  little speed (about 0.3 ms per mutant on `astutil`), so `-mcdc` is off by
  default.
- **Equivalent mutants** remain apart from the simple cases above. They are
  the real remaining problem — run time is not anymore.

Possible next steps: subtest selection, GitHub annotations or SARIF output for
pull requests, `//mutonce:ignore` comments, ranking bypassable checks by kind,
and measuring how productive ERR and SEL are compared to the classic operators.

## Background and references

- R. Untch, A. J. Offutt, M. J. Harrold: *Mutation Analysis Using Mutant
  Schemata*, ISSTA 1993.
- A. J. Offutt, A. Lee, G. Rothermel, R. Untch, C. Zapf: *An Experimental
  Determination of Sufficient Mutant Operators*, TOSEM 1996.
- G. Kaminski, P. Ammann, J. Offutt: *Improving Logic-Based Testing*, JSS 2013.
- R. Just, G. M. Kapfhammer, F. Schweiggert: *Using Non-Redundant Mutation
  Operators and Test Suite Prioritization to Achieve Efficient and Scalable
  Mutation Analysis*, ISSRE 2012.
- R. Just et al.: *Are Mutants a Valid Substitute for Real Faults in Software
  Testing?*, FSE 2014.
- R. Gopinath et al.: *On the Limits of Mutation Reduction Strategies*, ICSE 2016.
- G. Petrović, M. Ivanković, G. Fraser, R. Just: *Practical Mutation Testing at
  Scale: A view from Google*, TSE 2021.
- O. L. Vera-Pérez, M. Monperrus, B. Baudry: *Descartes: A PITest Engine to
  Detect Pseudo-Tested Methods*, ASE 2018.
- T. Tu, X. Liu, L. Song, Y. Zhang: *Understanding Real-World Concurrency Bugs
  in Go*, ASPLOS 2019.
- J. J. Chilenski, S. P. Miller: *Applicability of Modified Condition/Decision
  Coverage to Software Testing*, Software Engineering Journal 1994.
- J. J. Chilenski: *An Investigation of Three Forms of the Modified Condition
  Decision Coverage (MCDC) Criterion*, FAA report DOT/FAA/AR-01/18, 2001.

## Project layout

- [cmd/mutonce](cmd/mutonce): command line.
- [internal/mutant](internal/mutant): operators, selection of mutation points,
  AST rewriting, type-check filter, runtime helpers, diff selection, MC/DC
  instrumentation and analysis ([mcdc.go](internal/mutant/mcdc.go)).
- [internal/run](internal/run): builds, per-test coverage, execution,
  sandboxes, platform guards, naive comparison mode, report.
- [example](example): an example module with deliberately weak tests,
  concurrency, endless-loop and memory-hungry mutants, and corner cases
  ([tricky](example/tricky/tricky.go)).

## License

[MIT](LICENSE)
