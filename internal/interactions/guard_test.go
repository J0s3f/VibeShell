package interactions_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenNames are the program names the acceptance examples imitate. They may
// appear in tests and in fixtures, where describing an example is the point. They
// must not appear in the runtime package: a binding maps a key to an approved
// primitive action, so the set of supported interactions is the set of actions,
// not a list of program names.
var forbiddenNames = []string{"vi", "vim", "less", "more", "top", "htop", "nano", "emacs"}

// TestNoProgramNameUniverse is a hermetic source check: every string literal in
// the runtime package is scanned for a program name. A switch on "vi" or a
// special-case table for "less" would be the failure this prevents, and it is
// the cheapest place to catch it, because such a table has no other legitimate
// reason to exist.
func TestNoProgramNameUniverse(t *testing.T) {
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			for _, forbidden := range forbiddenNames {
				if mentionsProgram(value, forbidden) {
					t.Errorf("%s names the program %q in a string literal: %q",
						name, forbidden, value)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no runtime files were checked; the guard would pass vacuously")
	}
}

// mentionsProgram reports whether a string literal is exactly a program name. A
// substring match would flag ordinary words: the scroll unit "top" and the
// program top have nothing to do with each other. The failure this guard exists
// to catch is a literal that names a program to branch on, and a branch on a
// program name needs the whole literal.
func mentionsProgram(value, program string) bool {
	return strings.EqualFold(strings.TrimSpace(value), program)
}

// forbiddenImports are the packages that would give the primitives a way to
// touch the outside world. A primitive that could read a file, open a socket,
// read a clock, or start a process would no longer be a screen operation, so the
// dependency is refused at the source level rather than reviewed.
var forbiddenImports = map[string]bool{
	"os":            true,
	"io":            true,
	"bufio":         true,
	"net":           true,
	"net/http":      true,
	"database/sql":  true,
	"os/exec":       true,
	"syscall":       true,
	"runtime":       true,
	"time":          true,
	"math/rand":     true,
	"crypto/rand":   true,
	"context":       true,
	"os/user":       true,
	"runtime/debug": true,
}

// TestNoPrimitiveSideEffects is a hermetic source check: the runtime package may
// import only the standard library and the domain. Time, randomness, sessions,
// and storage are injected by the caller, so every function here stays
// deterministic and offline.
func TestNoPrimitiveSideEffects(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		path := filepath.Join(".", name)
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, imported := range file.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("unquoting import in %s: %v", path, err)
			}
			if forbiddenImports[value] {
				t.Errorf("%s imports %q; a primitive must not reach the outside world", name, value)
			}
			if strings.HasPrefix(value, "j0s.at/") && value != "j0s.at/vibeshell/internal/domain" {
				t.Errorf("%s imports %q; only the domain is allowed inside the module", name, value)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no runtime files were checked; the guard would pass vacuously")
	}
}

// TestFixturesAreReachableFromTheTestSuite keeps the checked-in examples honest:
// every directory under the fixture root must be loaded by a test.
func TestFixturesAreReachableFromTheTestSuite(t *testing.T) {
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("reading %s: %v", fixtureDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no interaction fixtures are checked in")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			loadExample(t, name)
		})
	}
}
