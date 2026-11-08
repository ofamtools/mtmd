//go:build ignore

// ofam-prune deletes everything from the tree that the daemon facade
// (./serve) does not need, so the fork holds only tuios's daemon and the
// packages it links. It is the second half of the merge recipe in FORK.md:
// after `git merge vX.Y.Z` the files upstream changed in packages that were
// pruned come back as modify/delete conflicts, and this script resolves
// them the same way each time.
//
// Usage (from the module root):
//
//	GOWORK=off go run scripts/ofam-prune.go          # delete what is not kept
//	GOWORK=off go run scripts/ofam-prune.go -check   # list it, exit 1 if any
//	GOWORK=off go run scripts/ofam-prune.go -list    # print the kept packages
//	GOWORK=off go run scripts/ofam-prune.go -tidy    # also run go mod tidy
//
// The keep rule (see FORK.md):
//
//  1. Roots are the packages of ./serve. The kept set is their dependencies
//     in this module, for linux, darwin and windows, plus the test
//     dependencies of every kept package, to a fixed point.
//  2. A file is kept when the nearest directory at or above it that holds Go
//     files is a kept package. That keeps testdata, embedded files, LICENSE
//     and NOTICE files that sit with kept code, and drops the rest.
//  3. The licence and notice files of the upstream-owned modules the build
//     still links (sketch, ... ; not vt, which is the in-tree internal/vt
//     made by ofam-vt.go and keeps its own LICENSE and NOTICE by rule 2) are copied from the module
//     cache into third_party/MODULE/ before their in-tree copies go, so
//     every notice that applies to code the daemon links stays in the tree.
//  4. A file that Go code of a kept package names by a "../"-relative string
//     literal (a test reading ../../docs/CLI_REFERENCE.md) is kept too.
//  5. The fork's own root files are kept by name (rootKeep) and so are
//     scripts/ofam-*.go, and third_party/ is generated. Everything else is deleted, including every nested
//     module (vt, sketch, loom, ...: go.mod pins them at published versions
//     that are identical to the in-tree copies), go.work and the docs.
//
// It never touches .git (a file in a linked worktree) or .worktrees.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const modulePath = "ofam.dev/mtmd"

// ./internal/coldlog is not linked by serve until the wiring lane (M3) lands;
// a root keeps it from being pruned meanwhile.
var roots = []string{"./serve", "./internal/coldlog"}

// platforms are the operating systems whose build constraints widen the kept set.
var platforms = []string{"linux", "darwin", "windows"}

// rootKeep are the files at the module root that stay. LICENSE is upstream's
// and must travel with every copy; NOTICE and README carry the credit.
var rootKeep = map[string]bool{
	"LICENSE": true, "NOTICE": true, "README.md": true, "FORK.md": true, "THIRD_PARTY.md": true,
	"go.mod": true, "go.sum": true, ".gitignore": true, ".gitattributes": true,
}

// scriptData are the non-Go files of scripts/ that stay: the pinned version
// and the patch that ofam-vt.go applies.
var scriptData = map[string]bool{"vt-version": true, "vt-ofam.patch": true}

func main() {
	check := flag.Bool("check", false, "list what would be deleted and exit 1 if anything")
	list := flag.Bool("list", false, "print the kept packages and exit")
	tidy := flag.Bool("tidy", false, "run go mod tidy after pruning")
	flag.Parse()

	kept, err := keptPackages()
	if err != nil {
		fatal(err)
	}
	if *list {
		for _, p := range sorted(kept) {
			fmt.Println(p)
		}
		return
	}
	keptDirs := map[string]bool{}
	for p := range kept {
		keptDirs[strings.TrimPrefix(strings.TrimPrefix(p, modulePath), "/")] = true
	}
	// "" is the module root: not a package here, never kept as one.
	delete(keptDirs, "")

	var goDirs = map[string]bool{}
	var files []string
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// .git is a file in a linked worktree: never touch it, whatever it is.
		if path == ".git" || path == ".worktrees" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		path = filepath.ToSlash(path)
		files = append(files, path)
		if strings.HasSuffix(path, ".go") {
			goDirs[filepath.ToSlash(filepath.Dir(path))] = true
		}
		return nil
	})
	if err != nil {
		fatal(err)
	}

	tp, err := thirdParty()
	if err != nil {
		fatal(err)
	}
	var stale []string
	for dst, src := range tp {
		want, err := os.ReadFile(src)
		if err != nil {
			fatal(err)
		}
		if have, err := os.ReadFile(dst); err != nil || !bytes.Equal(have, want) {
			stale = append(stale, dst)
			if !*check {
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					fatal(err)
				}
				if err := os.WriteFile(dst, want, 0o644); err != nil {
					fatal(err)
				}
			}
		}
	}
	sort.Strings(stale)
	named := namedFiles(keptDirs)
	var doomed []string
	for _, f := range files {
		if named[f] {
			continue
		}
		if strings.HasPrefix(f, "third_party/") {
			if _, ok := tp[f]; !ok {
				doomed = append(doomed, f) // a notice of a module no longer linked
			}
			continue
		}
		if !keep(f, keptDirs, goDirs) {
			doomed = append(doomed, f)
		}
	}
	if *check {
		for _, f := range doomed {
			fmt.Println(f)
		}
		for _, f := range stale {
			fmt.Println("write", f)
		}
		if len(doomed)+len(stale) > 0 {
			fmt.Fprintf(os.Stderr, "ofam-prune: %d files would be deleted, %d third_party files written\n", len(doomed), len(stale))
			os.Exit(1)
		}
		return
	}
	for _, f := range doomed {
		if err := os.Remove(f); err != nil {
			fatal(err)
		}
	}
	removeEmptyDirs(".")
	fmt.Printf("ofam-prune: %d files deleted, %d third_party files written, %d packages kept\n", len(doomed), len(stale), len(kept))
	if *tidy {
		c := exec.Command("go", "mod", "tidy")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fatal(err)
		}
	}
}

func keep(f string, keptDirs, goDirs map[string]bool) bool {
	if !strings.Contains(f, "/") {
		return rootKeep[f]
	}
	if strings.HasPrefix(f, "third_party/") {
		return true
	}
	if strings.HasPrefix(f, "scripts/") {
		base := filepath.Base(f)
		return (strings.HasPrefix(base, "ofam-") || scriptData[base]) && !strings.Contains(strings.TrimPrefix(f, "scripts/"), "/")
	}
	for dir := filepath.ToSlash(filepath.Dir(f)); dir != "." && dir != "/"; dir = filepath.ToSlash(filepath.Dir(dir)) {
		if goDirs[dir] || keptDirs[dir] {
			return keptDirs[dir]
		}
	}
	return false
}

// keptPackages is the closure described in the package comment.
func keptPackages() (map[string]bool, error) {
	kept := map[string]bool{}
	targets := append([]string(nil), roots...)
	for {
		before := len(kept)
		for _, goos := range platforms {
			// -test adds the test dependencies of the packages named.
			out, err := goList(goos, append([]string{"list", "-deps", "-test", "-f", "{{.ImportPath}}"}, targets...)...)
			if err != nil {
				return nil, err
			}
			sc := bufio.NewScanner(bytes.NewReader(out))
			for sc.Scan() {
				p, _, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
				p = strings.TrimSuffix(strings.TrimSuffix(p, ".test"), "_test")
				if p == modulePath || strings.HasPrefix(p, modulePath+"/") {
					kept[p] = true
				}
			}
		}
		if len(kept) == before {
			return kept, nil
		}
		targets = sorted(kept)
	}
}

func goList(goos string, args ...string) ([]byte, error) {
	c := exec.Command("go", args...)
	c.Env = append(os.Environ(), "GOWORK=off", "GOOS="+goos, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s (GOOS=%s): %v\n%s", strings.Join(args[:2], " "), goos, err, stderr.String())
	}
	return out, nil
}

func removeEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			if p == ".git" || p == ".worktrees" {
				return filepath.SkipDir
			}
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		if dirs[i] != "." {
			_ = os.Remove(dirs[i]) // fails when not empty
		}
	}
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ofam-prune:", err)
	os.Exit(1)
}

// noticeFile matches the names of licence and notice files.
var noticeFile = regexp.MustCompile(`(?i)^(LICENSE|LICENCE|NOTICE|COPYING|OFL)([-._].*)?$`)

// thirdParty maps third_party/MODULE/PATH to the file in the module cache it
// is copied from, for every tuios.dev module the build links (these are the
// upstream repository's own nested modules, pinned in go.mod). Test data is
// not linked and is skipped.
func thirdParty() (map[string]string, error) {
	dirs := map[string]string{}
	for _, goos := range platforms {
		out, err := goList(goos, "list", "-deps", "-f", "{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}|{{.Module.Dir}}{{end}}{{end}}", "./serve")
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(string(out), "\n") {
			mod, dir, ok := strings.Cut(strings.TrimSpace(l), "|")
			if ok && strings.HasPrefix(mod, "tuios.dev/") && dir != "" {
				dirs[mod] = dir
			}
		}
	}
	res := map[string]string{}
	for mod, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if noticeFile.MatchString(d.Name()) {
				rel, _ := filepath.Rel(dir, p)
				res[filepath.ToSlash(filepath.Join("third_party", mod, rel))] = p
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

var relLiteral = regexp.MustCompile(`"((?:\.\./)+[A-Za-z0-9_][A-Za-z0-9_./-]*)"`)

// namedFiles are the existing files that the Go files of kept packages name by
// a "../"-relative string literal, resolved from the package directory.
func namedFiles(keptDirs map[string]bool) map[string]bool {
	out := map[string]bool{}
	for dir := range keptDirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			for _, m := range relLiteral.FindAllSubmatch(src, -1) {
				p := filepath.ToSlash(filepath.Clean(filepath.Join(dir, string(m[1]))))
				if st, err := os.Stat(p); err == nil && !st.IsDir() && !strings.HasPrefix(p, "../") {
					out[p] = true
				}
			}
		}
	}
	return out
}
