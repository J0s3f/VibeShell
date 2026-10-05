package sshserver_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The VibeShell contract says no SSH request, terminal key, or decoded paste
// may ever reach an operating-system process. The design argument is in the
// sshserver package documentation; this test is the mechanical half, and it
// fails if any file in the spike module grows an import or a call that could
// start one, test helpers included.

// forbiddenImports may not appear in any file of the module.
var forbiddenImports = map[string]string{
	"os/exec":          "exec.Command starts a process",
	"syscall":          "syscall.ForkExec, syscall.Exec, and syscall.StartProcess start a process",
	"runtime/cgo":      "cgo could call a process launcher",
	"golang.org/x/sys": "wraps syscalls, including process creation",
}

// processStartingCalls may not be referenced on an otherwise allowed import.
// The os package itself stays allowed: the spike reads its host key file.
var processStartingCalls = map[string]map[string]string{
	"os": {
		"StartProcess": "starts a process",
		"FindProcess":  "looks up a running process",
		"Process":      "holds a process handle",
	},
	"os/exec": {
		"Command":        "builds a command to run",
		"CommandContext": "builds a command to run",
	},
	"syscall": {
		"Exec":         "replaces the running process image",
		"ForkExec":     "forks and executes",
		"StartProcess": "starts a process",
	},
}

// moduleRoot is the spike module, relative to this package.
const moduleRoot = "../.."

func TestNoProcessSpawningPrimitives(t *testing.T) {
	files := 0
	err := filepath.WalkDir(moduleRoot, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		files++
		return checkFileForProcesses(t, name)
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	if files < 5 {
		t.Fatalf("only %d Go files were inspected; the check is not doing anything", files)
	}
	t.Logf("checked %d Go files for process-spawning primitives", files)
}

func checkFileForProcesses(t *testing.T, name string) error {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, name, nil, 0)
	if err != nil {
		return err
	}
	// localNames maps the identifier a file uses for an import to its path.
	localNames := map[string]string{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return err
		}
		if reason, forbidden := forbiddenImports[importPath]; forbidden {
			t.Errorf("%s imports %q, which can %s", name, importPath, reason)
		}
		local := path.Base(importPath)
		if spec.Name != nil {
			local = spec.Name.Name
		}
		localNames[local] = importPath
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		importPath, imported := localNames[identifier.Name]
		if !imported {
			return true
		}
		reason, forbidden := processStartingCalls[importPath][selector.Sel.Name]
		if forbidden {
			t.Errorf("%s uses %s.%s, which %s", name, importPath, selector.Sel.Name, reason)
		}
		return true
	})
	return nil
}
