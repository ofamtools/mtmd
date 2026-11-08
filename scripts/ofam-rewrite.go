//go:build ignore

// ofam-rewrite moves this fork's module path from github.com/Gaurav-Gosain/tuios
// to ofam.dev/mtmd. Run it from the repository root after every upstream
// merge: it is idempotent, and `go run scripts/ofam-rewrite.go -check` exits 1
// while any module-path reference is left.
//
// What it rewrites, and what it leaves:
//
//   - go.mod and go.work files: every reference, so module, require and replace
//     lines follow.
//   - Go import specs (`"github.com/Gaurav-Gosain/tuios/..."` on a line of its
//     own, with or without an alias).
//   - Go imports of the packages in movedImports (modules whose source now
//     lives in this module, see ofam-vt.go): one line each, same package name.
//   - Files named in pathStrings: tests that hand the module path to `go list`.
//
// It leaves URLs (the GitHub repository is still upstream's), install scripts,
// release notes, and the gob type name registered in internal/session/codec.go:
// that string is on disk in saved state and in the wire protocol, so changing
// it would make every saved terminal state unreadable.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	oldPath = "github.com/Gaurav-Gosain/tuios"
	newPath = "ofam.dev/mtmd"
)

// pathStrings are Go files whose string literals name packages of the module
// and must follow the rewrite (matched by base name).
var pathStrings = map[string]bool{
	"imports_test.go":   true,
	"childproc_test.go": true,
}

// movedImports are import paths that now name a package of this module: the
// module on the left (and its subpackages) is carried in-tree at the right.
var movedImports = []struct{ from, to string }{
	{"tuios.dev/vt", "ofam.dev/mtmd/internal/vt"},
}

var (
	// the old path followed by a boundary: not another path element such as
	// tuios-web or tuios.dev.
	pathRe = regexp.MustCompile(regexp.QuoteMeta(oldPath) + `($|[^A-Za-z0-9_.\-])`)
	// "github.com/..." alone on a line, with an optional alias.
	importRe = regexp.MustCompile(`^\s*([A-Za-z_.][A-Za-z0-9_]*\s+)?"` + regexp.QuoteMeta(oldPath) + `(/[^"]*)?"\s*(//.*)?$`)
	// a quoted import of a moved module, alone on a line.
	movedRe = regexp.MustCompile(`^(\s*(?:[A-Za-z_.][A-Za-z0-9_]*\s+)?)"([^"]+)"(\s*(?://.*)?)$`)
	// a URL or a git remote carries the same text and is not a module path.
	urlBefore = regexp.MustCompile(`(://|@|github\.com:)$`)
)

func main() {
	check := flag.Bool("check", false, "list remaining module-path references and exit 1 when any are left")
	flag.Parse()
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git ls-files:", err)
		os.Exit(2)
	}
	changed, left := 0, 0
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" || name == "scripts/ofam-rewrite.go" {
			continue
		}
		base := filepath.Base(name)
		isMod := base == "go.mod" || base == "go.work"
		isGo := strings.HasSuffix(name, ".go")
		if !isMod && !isGo {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		dirty := false
		for i, ln := range lines {
			if isGo {
				if nl, ok := moveImport(ln); ok {
					if *check {
						fmt.Printf("%s:%d: %s\n", name, i+1, strings.TrimSpace(ln))
						left++
						continue
					}
					lines[i] = nl
					dirty = true
					continue
				}
			}
			if !strings.Contains(ln, oldPath) {
				continue
			}
			if isGo && strings.Contains(ln, ".TerminalState") {
				continue // persisted gob name
			}
			if isMod || (isGo && (importRe.MatchString(ln) || pathStrings[base])) {
				if *check {
					fmt.Printf("%s:%d: %s\n", name, i+1, strings.TrimSpace(ln))
					left++
					continue
				}
				lines[i] = rewrite(ln)
				dirty = true
			}
		}
		if dirty {
			if err := os.WriteFile(name, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			changed++
		}
	}
	if *check {
		if left > 0 {
			os.Exit(1)
		}
		return
	}
	// import blocks re-sort under the new prefix
	if changed > 0 {
		var files bytes.Buffer
		files.WriteString("")
		_ = files
		cmd := exec.Command("sh", "-c", "git ls-files -z '*.go' | xargs -0 gofmt -l -w >/dev/null")
		_ = cmd.Run()
	}
	fmt.Printf("ofam-rewrite: %d files changed\n", changed)
}

// moveImport rewrites an import line of a moved module.
func moveImport(ln string) (string, bool) {
	m := movedRe.FindStringSubmatch(ln)
	if m == nil {
		return "", false
	}
	for _, r := range movedImports {
		if m[2] == r.from || strings.HasPrefix(m[2], r.from+"/") {
			return m[1] + `"` + r.to + strings.TrimPrefix(m[2], r.from) + `"` + m[3], true
		}
	}
	return "", false
}

func rewrite(ln string) string {
	var b strings.Builder
	rest := ln
	for {
		loc := pathRe.FindStringIndex(rest)
		if loc == nil {
			b.WriteString(rest)
			return b.String()
		}
		start := loc[0]
		// keep the boundary byte (the second group) in rest
		end := start + len(oldPath)
		if urlBefore.MatchString(rest[:start]) {
			b.WriteString(rest[:end])
		} else {
			b.WriteString(rest[:start])
			b.WriteString(newPath)
		}
		rest = rest[end:]
	}
}
