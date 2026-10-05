// Command inspectimports lists the imports and exports of the pinned
// QuickJS wasm module so the capability-free host module can be built
// against the exact set of WASI symbols the guest requests.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func typeName(t api.ValueType) string {
	switch t {
	case api.ValueTypeI32:
		return "i32"
	case api.ValueTypeI64:
		return "i64"
	case api.ValueTypeF32:
		return "f32"
	case api.ValueTypeF64:
		return "f64"
	default:
		return fmt.Sprintf("?%d", t)
	}
}

func main() {
	wasmBytes, err := os.ReadFile("qjs.wasm")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read qjs.wasm:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "compile:", err)
		os.Exit(1)
	}

	fmt.Println("== imports (module.name : params -> results) ==")
	for _, imp := range compiled.ImportedFunctions() {
		moduleName, name, isImport := imp.Import()
		if !isImport {
			continue
		}
		params := make([]string, len(imp.ParamTypes()))
		for i, t := range imp.ParamTypes() {
			params[i] = typeName(t)
		}
		results := make([]string, len(imp.ResultTypes()))
		for i, t := range imp.ResultTypes() {
			results[i] = typeName(t)
		}
		fmt.Printf("  %s.%s : %s -> %s\n", moduleName, name,
			join(params), join(results))
	}
}

func join(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
