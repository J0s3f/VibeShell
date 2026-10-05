package domain_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// deniedStdlibImports are standard-library packages that would still break
// the hexagonal boundary (network, SQL, process execution). Contracts must
// not touch them even though they carry no third-party module path.
var deniedStdlibImports = map[string]bool{
	"net":          true,
	"net/http":     true,
	"database/sql": true,
	"os/exec":      true,
	"syscall":      true,
}

// TestArchitectureGuard is a hermetic, file-based dependency check: it reads
// the .go sources under internal/domain and internal/ports and fails on any
// non-stdlib import (first path segment containing a dot, i.e. an external
// module), any denied stdlib transport/storage import, and any intra-repo
// import that violates the dependency direction (domain must import nothing
// from the module; ports may import only the domain).
func TestArchitectureGuard(t *testing.T) {
	roots := map[string]func(path string) bool{
		"../../internal/domain": func(path string) bool {
			return false // domain imports nothing from this module
		},
		"../../internal/ports": func(path string) bool {
			return path == "j0s.at/vibeshell/internal/domain"
		},
	}
	for root, allowModule := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("reading %s: %v", root, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s contains no files", root)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			full := filepath.Join(root, name)
			src, err := os.ReadFile(full)
			if err != nil {
				t.Fatalf("reading %s: %v", full, err)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, full, src, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing %s: %v", full, err)
			}
			for _, imp := range f.Imports {
				raw, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("unquoting import in %s: %v", full, err)
				}
				rel, _ := filepath.Rel("../../..", full)
				first := strings.SplitN(raw, "/", 2)[0]
				switch {
				case strings.Contains(first, "."):
					// External module (github.com/…, golang.org/…, or the
					// j0s.at vanity path): only the domain import is
					// acceptable, and only inside ports.
					if !allowModule(raw) {
						t.Errorf("%s imports non-stdlib package %q", rel, raw)
					}
				case deniedStdlibImports[raw]:
					t.Errorf("%s imports adapter-level stdlib package %q", rel, raw)
				}
			}
		}
	}
}
