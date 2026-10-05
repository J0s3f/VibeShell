// Command debugeval probes the QJS eval path with raw values printed.
package main

import (
	"context"
	"fmt"

	"j0s.at/vibeshell/experiments/sandbox"
)

func main() {
	ctx := context.Background()
	engine, err := sandbox.NewEngine(ctx, sandbox.DefaultMemoryPages)
	if err != nil {
		panic(err)
	}
	defer engine.Close(ctx)

	inst, err := engine.NewInstance(ctx, sandbox.Config{})
	if err != nil {
		panic(err)
	}
	defer inst.Close()

	for _, code := range []string{"1+1", "'hello'", "JSON.stringify({a:1})"} {
		got, err := inst.Eval(ctx, code)
		fmt.Printf("Eval(%q) = %q err=%v\n", code, got, err)
	}
}
