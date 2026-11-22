package run

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

// Mutants change behaviour, and some of that behaviour writes files: a path
// mutated to "" turns `path + ".tmp"` into ".tmp" in the working directory.
// Run in the real source tree, every such run leaves files behind. So each
// worker gets its own copy of the module, test processes run in the package
// directory of that copy, and whatever a run adds there is removed again.

// symlinkFrom is the size above which files are linked rather than copied;
// large files are typically binaries or data that tests only read.
const symlinkFrom = 1 << 20

// linkedDirs are linked as a whole instead of being copied.
var linkedDirs = map[string]bool{"node_modules": true}

type sandboxes struct {
	module string
	free   chan string

	mu      sync.Mutex
	listing map[string]map[string]bool // package dir (relative) -> original entries
	effects map[string]bool            // relative paths of files tests left behind
}

func newSandboxes(module, work string, n int) (*sandboxes, error) {
	s := &sandboxes{module: module, free: make(chan string, n), listing: map[string]map[string]bool{}, effects: map[string]bool{}}
	for i := range n {
		dst := filepath.Join(work, "sandbox", "w"+strconv.Itoa(i))
		if err := copyTree(module, dst); err != nil {
			return nil, err
		}
		s.free <- dst
	}
	return s, nil
}

// copyTree mirrors src into dst: directories and small files are copied,
// large files, node_modules and symlinks are linked, .git is left out.
// Where symlinks are not permitted, large files are copied as well.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir() && d.Name() == ".git" && p != src:
			return filepath.SkipDir
		case d.IsDir() && linkedDirs[d.Name()]:
			// Without symlinks (Windows without developer mode) such a
			// directory is left out rather than copied; Go tests rarely need it.
			_ = os.Symlink(p, target)
			return filepath.SkipDir
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.Symlink(link, target) // see above
			return nil
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > symlinkFrom && os.Symlink(p, target) == nil {
			return nil
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// run executes f with the working directory of pkgDir inside a free copy
// and then removes the entries f added to that directory.
func (s *sandboxes) run(pkgDir string, f func(dir string) testRun) testRun {
	box := <-s.free
	defer func() { s.free <- box }()
	rel, err := filepath.Rel(s.module, pkgDir)
	if err != nil {
		return testRun{err: err}
	}
	dir := filepath.Join(box, rel)
	orig := s.original(rel)
	run := f(dir)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !orig[e.Name()] {
			s.mu.Lock()
			s.effects[filepath.Join(rel, e.Name())] = true
			s.mu.Unlock()
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return run
}

// original lists the package directory as it is in the module.
func (s *sandboxes) original(rel string) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.listing[rel]; ok {
		return l
	}
	l := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(s.module, rel))
	for _, e := range entries {
		l[e.Name()] = true
	}
	s.listing[rel] = l
	return l
}

// sideEffects lists the files test runs left behind, relative to the module.
func (s *sandboxes) sideEffects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.effects))
	for p := range s.effects {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
