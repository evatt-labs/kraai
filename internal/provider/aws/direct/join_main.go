//go:build ignore

// join_main drafts an override for every CloudFormation type Join can join
// to its model mechanically, and reports why each other type could not be.
// Offline: it reads a local checkout of api-models-aws and a directory of
// raw CloudFormation schemas, such as the schema index's cache.
//
//	go run join_main.go -models ~/api-models-aws/models -schemas ~/.cache/kraai/cfschema/us-east-1 [-write]
//
// With -write it writes each joined type's override, never replacing one
// already checked in: those are reviewed, and a person's mapping wins.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

// joinedHeader opens every override this tool writes. A person reviewing
// one deletes it, and from then on the tool leaves the file alone.
const joinedHeader = "# Joined mechanically by join_main.go; delete this line once reviewed.\n"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "join:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		modelsDir  = flag.String("models", "", "the models/ directory of an api-models-aws checkout")
		schemasDir = flag.String("schemas", "", "a directory of raw CloudFormation schemas, one per type")
		write      = flag.Bool("write", false, "write each joined type's override under overrides/")
		reasons    = flag.Bool("reasons", false, "list every unjoined type with its reasons")
	)
	flag.Parse()
	if *modelsDir == "" || *schemasDir == "" {
		return fmt.Errorf("-models and -schemas are required")
	}

	var models []direct.JoinModel
	err := filepath.WalkDir(*modelsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(*modelsDir, p)
		if err != nil {
			return err
		}
		models = append(models, direct.JoinModel{Path: filepath.ToSlash(rel), Raw: raw})
		return nil
	})
	if err != nil {
		return err
	}
	names, err := filepath.Glob(filepath.Join(*schemasDir, "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(names)
	var schemas [][]byte
	for _, name := range names {
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		schemas = append(schemas, raw)
	}

	joined, err := direct.Join(models, schemas)
	if err != nil {
		return err
	}
	// A file without the joined header was written or edited by a person.
	reviewed := map[string]bool{}
	existing, err := filepath.Glob("overrides/*.yaml")
	if err != nil {
		return err
	}
	for _, name := range existing {
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(raw, []byte(joinedHeader)) {
			reviewed[strings.ReplaceAll(strings.TrimSuffix(filepath.Base(name), ".yaml"), "--", "::")] = true
		}
	}

	byProtocol, byReason := map[string]int{}, map[string]int{}
	written := 0
	for _, j := range joined {
		if j.Override == nil {
			for _, r := range j.Reasons {
				byReason[reasonClass(r)]++
			}
			if *reasons {
				fmt.Printf("%s\t%s\t%s\n", j.Type, j.Protocol, strings.Join(j.Reasons, "; "))
			}
			continue
		}
		byProtocol[j.Protocol]++
		if !*write || reviewed[j.Type] {
			continue
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(j.Override); err != nil {
			return err
		}
		file := "overrides/" + strings.ReplaceAll(j.Type, "::", "--") + ".yaml"
		if err := os.WriteFile(file, append([]byte(joinedHeader), buf.Bytes()...), 0o644); err != nil {
			return err
		}
		written++
	}

	total := 0
	for _, n := range byProtocol {
		total += n
	}
	fmt.Printf("%d of %d types joined\n", total, len(joined))
	for _, p := range sortedKeys(byProtocol) {
		fmt.Printf("  %-12s %d\n", p, byProtocol[p])
	}
	fmt.Println("not joined, by first reason:")
	for _, r := range sortedKeys(byReason) {
		fmt.Printf("  %5d  %s\n", byReason[r], r)
	}
	if *write {
		fmt.Printf("wrote %d overrides\n", written)
	}
	return nil
}

// reasonClass drops a reason's specifics, so reasons count by kind.
func reasonClass(r string) string {
	if i := strings.IndexByte(r, ':'); i >= 0 {
		return r[:i]
	}
	return r
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
