package run

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCgroupAvailable(t *testing.T) {
	root := t.TempDir()
	write := func(rel, data string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A job limited to 8 GiB with 1 GiB in use, inside a slice with 2 GiB
	// left and an unlimited root.
	write("memory.max", "max\n")
	write("ci.slice/memory.max", "4294967296\n")
	write("ci.slice/memory.current", "2147483648\n")
	write("ci.slice/job.scope/memory.max", "8589934592\n")
	write("ci.slice/job.scope/memory.current", "1073741824\n")
	proc := filepath.Join(t.TempDir(), "cgroup")

	for _, tc := range []struct {
		cgroup string
		want   int64
		ok     bool
	}{
		{"0::/ci.slice/job.scope\n", 2 << 30, true}, // the tighter parent wins
		{"0::/\n", 0, false},                        // no limit at the root
		{"0::/../../other.scope\n", 0, false},       // outside the namespace's view
	} {
		if err := os.WriteFile(proc, []byte(tc.cgroup), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := cgroupAvailable(proc, root)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: %d, %v; want %d, %v", tc.cgroup, got, ok, tc.want, tc.ok)
		}
	}
}
