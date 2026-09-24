//go:build ignore

// propose_main drafts the mappings of each named override in place, for a
// person to review before it is checked in.
//
//	go run propose_main.go AWS::XRay::Group [...]
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "propose:", err)
		os.Exit(1)
	}
}

func run(types []string) error {
	all, err := direct.Overrides()
	if err != nil {
		return err
	}
	for _, typeName := range types {
		var found bool
		for _, o := range all {
			if o.Type != typeName {
				continue
			}
			found = true
			drafted, err := direct.Propose(o)
			if err != nil {
				return fmt.Errorf("%s: %w", typeName, err)
			}
			var buf bytes.Buffer
			enc := yaml.NewEncoder(&buf)
			enc.SetIndent(2)
			if err := enc.Encode(drafted); err != nil {
				return err
			}
			file := "overrides/" + strings.ReplaceAll(typeName, "::", "--") + ".yaml"
			if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
				return err
			}
			fmt.Println("drafted", file)
		}
		if !found {
			return fmt.Errorf("%s has no override", typeName)
		}
	}
	return nil
}
