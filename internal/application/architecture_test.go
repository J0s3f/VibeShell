package application

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// deniedImports are the standard-library packages that would break the hexagonal
// boundary even though they carry no third-party module path. The application
// layer coordinates use cases, so it must not reach for transport, storage, or
// process execution directly.
var deniedImports = map[string]bool{
	"net":          true,
	"net/http":     true,
	"database/sql": true,
	"os/exec":      true,
	"syscall":      true,
}

// TestApplicationDependsOnlyInwards is a hermetic, file-based dependency check:
// the application package may import the domain, the ports, and the standard
// library, and nothing else. An outbound adapter, a provider SDK, or a terminal
// library appearing here would invert the dependency direction of PLAN 3.3.
func TestApplicationDependsOnlyInwards(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolving the package directory: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}

	allowed := map[string]bool{
		"j0s.at/vibeshell/internal/domain": true,
		"j0s.at/vibeshell/internal/ports":  true,
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		full := filepath.Join(root, name)
		src, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("reading %s: %v", full, err)
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, full, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", full, err)
		}
		for _, imported := range parsed.Imports {
			raw, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("unquoting import in %s: %v", full, err)
			}
			if deniedImports[raw] {
				t.Errorf("%s imports adapter-level package %q", name, raw)
				continue
			}
			if !allowed[raw] && strings.Contains(strings.SplitN(raw, "/", 2)[0], ".") {
				t.Errorf("%s imports %q: the application layer may use only internal/domain and internal/ports", name, raw)
			}
		}
	}
}
