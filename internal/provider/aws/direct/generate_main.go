//go:build ignore

// generate_main writes readers.go from the checked-in overrides and subset.
// Offline: it needs neither the network nor credentials.
//
//	go run generate_main.go
package main

import (
	"fmt"
	"os"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

func main() {
	src, err := direct.Generate()
	if err == nil {
		err = os.WriteFile("readers.go", src, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate:", err)
		os.Exit(1)
	}
}
