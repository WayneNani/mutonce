package mutant

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Changes maps absolute file paths to the line ranges changed in them.
type Changes map[string][][2]int

// Lines counts the changed lines.
func (c Changes) Lines() int {
	n := 0
	for _, rs := range c {
		for _, r := range rs {
			if r[1] == math.MaxInt {
				n++ // a new file counts as one change, not as MaxInt lines
				continue
			}
			n += r[1] - r[0] + 1
		}
	}
	return n
}

// Touches reports whether the mutant's code overlaps a changed line. A RET
// mutant spans its whole function, so any change inside the function
// selects it; expression mutants only match their own lines.
func (c Changes) Touches(m Mutant) bool {
	for _, r := range c[m.File] {
		if r[0] <= m.EndLine && r[1] >= m.Line {
			return true
		}
	}
	return false
}

var hunk = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ChangedLines lists the lines that differ between ref and the working tree
// under dir, including uncommitted changes and untracked files.
func ChangedLines(dir, ref string) (Changes, error) {
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	root = strings.TrimSpace(root)
	diff, err := git(dir, "-c", "core.quotePath=false", "diff", "--unified=0", "--no-color", "--no-ext-diff", ref, "--", ".")
	if err != nil {
		return nil, err
	}
	changes := Changes{}
	file := ""
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = ""
			if p, ok := strings.CutPrefix(line, "+++ b/"); ok && isSource(p) {
				file = filepath.Join(root, p)
			}
		case strings.HasPrefix(line, "@@") && file != "":
			m := hunk.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			if count == 0 {
				// Pure deletion: count the line the code was removed after.
				changes[file] = append(changes[file], [2]int{start, start})
				continue
			}
			changes[file] = append(changes[file], [2]int{start, start + count - 1})
		}
	}
	untracked, err := git(dir, "ls-files", "--others", "--exclude-standard", "--full-name", "--", ".")
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(untracked, "\n") {
		if isSource(p) {
			changes[filepath.Join(root, p)] = [][2]int{{1, math.MaxInt}}
		}
	}
	return changes, nil
}

// isSource reports whether a path can hold mutants: Go code, but no tests.
func isSource(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
