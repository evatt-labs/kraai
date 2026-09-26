//go:build ignore

// generate_main writes the readers_<service>.go files from the checked-in
// overrides and subset, removing any a service no longer has. Offline: it
// needs neither the network nor credentials.
//
//	go run generate_main.go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, "generate:", err)
		os.Exit(1)
	}
}

func generate() error {
	files, err := direct.Generate()
	if err != nil {
		return err
	}
	stale, err := filepath.Glob("readers_*.go")
	if err != nil {
		return err
	}
	for _, name := range stale {
		if _, kept := files[name]; !kept {
			if err := os.Remove(name); err != nil {
				return err
			}
		}
	}
	for name, src := range files {
		if err := os.WriteFile(name, src, 0o644); err != nil {
			return err
		}
	}
	return nil
}
