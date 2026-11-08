//go:build ignore

// ofam-vt makes internal/vt: a copy of the tuios.dev/vt module (the terminal
// emulator) at the version pinned in scripts/vt-version, with its self-imports
// moved to ofam.dev/mtmd/internal/vt and scripts/vt-ofam.patch applied on top.
// mtmd carries the emulator in its own module because a replace in mtmd's
// go.mod does not reach a program that imports mtmd, so a change to the
// emulator can only travel as mtmd's own code (FORK.md, patch #7).
//
// Usage (from the module root):
//
//	GOWORK=off go run scripts/ofam-vt.go          # regenerate internal/vt
//	GOWORK=off go run scripts/ofam-vt.go -check   # exit 1 if internal/vt differs
//
// The tree is rebuilt from scratch every time, so a hand edit of internal/vt is
// lost: put changes in scripts/vt-ofam.patch (a unified diff made from
// internal/vt, applied with `patch -p1` inside it). The script fails when the
// patch does not apply. An empty or absent patch is fine.
//
// To move the emulator to another version: write the new version into
// scripts/vt-version, run this script, run ofam-rewrite and ofam-prune, fix the
// patch if it no longer applies, and run the tests of internal/vt.
//
// Kept from the module: every file except go.mod and go.sum (the packages
// share mtmd's), including LICENSE, NOTICE, README.md, testdata and the vtgen
// and snapshot subpackages.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	modPath   = "tuios.dev/vt"
	dstDir    = "internal/vt"
	newPrefix = "ofam.dev/mtmd/internal/vt"
	pinFile   = "scripts/vt-version"
	patchFile = "scripts/vt-ofam.patch"
)

var skip = map[string]bool{"go.mod": true, "go.sum": true}

// a quoted import path of the module or one of its subpackages
var selfImport = regexp.MustCompile(`"` + regexp.QuoteMeta(modPath) + `(/[^"]*)?"`)

func main() {
	check := flag.Bool("check", false, "exit 1 when internal/vt differs from what this script generates")
	flag.Parse()

	pin, err := os.ReadFile(pinFile)
	if err != nil {
		fatal(err)
	}
	version := strings.TrimSpace(string(pin))
	src, err := moduleDir(version)
	if err != nil {
		fatal(err)
	}

	tmp, err := os.MkdirTemp("", "ofam-vt-")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(tmp)
	gen := filepath.Join(tmp, "vt")
	if err := copyTree(src, gen); err != nil {
		fatal(err)
	}
	if err := applyPatch(gen); err != nil {
		fatal(err)
	}

	if *check {
		out, _ := exec.Command("diff", "-rq", gen, dstDir).CombinedOutput()
		if len(out) > 0 || !exists(dstDir) {
			fmt.Printf("%s\nofam-vt: internal/vt differs from %s@%s + %s\n", out, modPath, version, patchFile)
			os.Exit(1)
		}
		return
	}
	if err := os.RemoveAll(dstDir); err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dstDir), 0o755); err != nil {
		fatal(err)
	}
	if err := copyTree(gen, dstDir); err != nil {
		fatal(err)
	}
	fmt.Printf("ofam-vt: %s from %s@%s\n", dstDir, modPath, version)
}

// moduleDir returns the module cache directory of the pinned version,
// downloading it when needed (go.mod need not require the module).
func moduleDir(version string) (string, error) {
	c := exec.Command("go", "mod", "download", "-json", modPath+"@"+version)
	c.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	var info struct{ Dir, Error string }
	_ = json.Unmarshal(out, &info)
	if err != nil || info.Dir == "" {
		return "", fmt.Errorf("go mod download %s@%s: %v %s %s", modPath, version, err, info.Error, stderr.String())
	}
	return info.Dir, nil
}

// copyTree copies src to dst without the skipped files, moving the self-imports
// of Go files outside testdata. Files are written writable (the module cache
// is read-only).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if skip[rel] {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(rel, ".go") && !strings.HasPrefix(rel, "testdata/") {
			data = rewriteImports(data)
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func rewriteImports(data []byte) []byte {
	if !selfImport.Match(data) {
		return data
	}
	out := selfImport.ReplaceAllFunc(data, func(m []byte) []byte {
		sub := selfImport.FindSubmatch(m)[1]
		return []byte(`"` + newPrefix + string(sub) + `"`)
	})
	if f, err := format.Source(out); err == nil {
		return f
	}
	return out
}

func applyPatch(dir string) error {
	p, err := os.ReadFile(patchFile)
	if err != nil || len(bytes.TrimSpace(p)) == 0 {
		return nil
	}
	abs, _ := filepath.Abs(patchFile)
	c := exec.Command("patch", "-p1", "--no-backup-if-mismatch", "-i", abs)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%s does not apply: %v\n%s", patchFile, err, out)
	}
	return nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ofam-vt:", err)
	os.Exit(1)
}
