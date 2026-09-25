package direct

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:generate go run generate_main.go

//go:embed lock.json models/*.json schemas/*.json overrides/*.yaml evidence/parity.json
var files embed.FS

// Lock records where the checked-in subset came from and what it hashed to,
// so a subset edited by hand, or regenerated from other inputs, is caught.
type Lock struct {
	SmithyCommit string `json:"smithyCommit"`
	// Models is keyed by the model's path under models/ in
	// github.com/aws/api-models-aws.
	Models map[string]LockedFile `json:"models"`
	// Schemas is keyed by CloudFormation type name.
	Schemas map[string]LockedFile `json:"schemas"`
}

// LockedFile is one extracted input: the hash of what was fetched, and the
// file and hash of what was checked in from it.
type LockedFile struct {
	File   string `json:"file"`
	Source string `json:"sourceSha256"`
	Subset string `json:"subsetSha256"`
}

// Overrides returns every override, sorted by type. Unknown fields are an
// error: a misspelled key must not be read as an omission.
func Overrides() ([]Override, error) { return overrides(files) }

func overrides(files fs.FS) ([]Override, error) {
	names, err := fs.Glob(files, "overrides/*.yaml")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]Override, 0, len(names))
	for _, name := range names {
		raw, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		var o Override
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&o); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if want := strings.ReplaceAll(o.Type, "::", "--") + ".yaml"; path.Base(name) != want {
			return nil, fmt.Errorf("%s: declares type %s, so it must be named %s", name, o.Type, want)
		}
		out = append(out, o)
	}
	return out, nil
}

// LoadLock returns the checked-in lock.
func LoadLock() (Lock, error) { return loadLock(files) }

func loadLock(files fs.FS) (Lock, error) {
	var l Lock
	raw, err := fs.ReadFile(files, "lock.json")
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(raw, &l)
}

// Verify checks that every checked-in model and schema hashes to what the
// lock recorded, and that every override's model and type are locked and
// its operation is in the model.
func Verify() error { return verify(files) }

func verify(files fs.FS) error {
	lock, err := loadLock(files)
	if err != nil {
		return err
	}
	check := func(what string, f LockedFile) error {
		raw, err := fs.ReadFile(files, f.File)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != f.Subset {
			return fmt.Errorf("%s: %s hashes to %s, the lock records %s; regenerate with go generate", what, f.File, got, f.Subset)
		}
		return nil
	}
	for model, f := range lock.Models {
		if err := check(model, f); err != nil {
			return err
		}
	}
	for typeName, f := range lock.Schemas {
		if err := check(typeName, f); err != nil {
			return err
		}
	}
	all, err := overrides(files)
	if err != nil {
		return err
	}
	for _, o := range all {
		locked, ok := lock.Models[o.Read.Model]
		if !ok {
			return fmt.Errorf("%s reads through %s, which the lock does not record", o.Type, o.Read.Model)
		}
		if _, ok := lock.Schemas[o.Type]; !ok {
			return fmt.Errorf("%s has no locked schema", o.Type)
		}
		if err := hasOperation(files, locked.File, o.Read.Operation); err != nil {
			return fmt.Errorf("%s: %w", o.Type, err)
		}
	}
	return nil
}

// hasOperation reports whether the model subset in file defines operation.
func hasOperation(files fs.FS, file, operation string) error {
	raw, err := fs.ReadFile(files, file)
	if err != nil {
		return err
	}
	var model struct {
		Shapes map[string]struct {
			Type string `json:"type"`
		} `json:"shapes"`
	}
	if err := json.Unmarshal(raw, &model); err != nil {
		return err
	}
	for id, shape := range model.Shapes {
		if shape.Type == "operation" && strings.HasSuffix(id, "#"+operation) {
			return nil
		}
	}
	return fmt.Errorf("%s defines no operation %s", file, operation)
}
