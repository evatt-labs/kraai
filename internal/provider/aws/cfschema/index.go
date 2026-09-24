package cfschema

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// ErrUnknownType is returned by Lookup for a type the index does not carry:
// a type outside the AWS:: namespace, one registered after the index was
// generated, or a misspelling.
var ErrUnknownType = errors.New("type is not in the schema index")

//go:generate go run gen.go -out index.json.gz

// indexData is the generated index: a gzipped JSON object mapping type
// name to Facts for every AWS-published FULLY_MUTABLE and IMMUTABLE public
// resource type in us-east-1 at generation time.
//
//go:embed index.json.gz
var indexData []byte

var (
	indexOnce sync.Once
	index     map[string]Facts
	errIndex  error
)

func loadIndex() (map[string]Facts, error) {
	indexOnce.Do(func() {
		index, errIndex = decodeIndex(indexData)
	})
	return index, errIndex
}

// DecodeIndex decodes an index in the format the generator writes.
func DecodeIndex(data []byte) (map[string]Facts, error) { return decodeIndex(data) }

func decodeIndex(data []byte) (map[string]Facts, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "opening embedded schema index")
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading embedded schema index")
	}
	var out map[string]Facts
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding embedded schema index")
	}
	return out, nil
}

// Lookup returns the indexed Facts for typeName, or ErrUnknownType.
func Lookup(typeName string) (Facts, error) {
	idx, err := loadIndex()
	if err != nil {
		return Facts{}, err
	}
	f, ok := idx[typeName]
	if !ok {
		return Facts{}, kerrors.Wrap(ErrUnknownType, kerrors.CodeValidation, "%s", typeName)
	}
	return f, nil
}

// Types returns every indexed type name, sorted.
func Types() ([]string, error) {
	idx, err := loadIndex()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(idx))
	for name := range idx {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// legacyData records, per type, the identities earlier indexes found it by,
// newest first, written by the generator when a change is accepted.
//
//go:embed legacy.json.gz
var legacyData []byte

var (
	legacyOnce sync.Once
	legacy     map[string][]Facts
	errLegacy  error
)

// Previous returns the identities earlier indexes found typeName by,
// newest first; empty for a type whose identity never changed.
func Previous(typeName string) ([]Facts, error) {
	legacyOnce.Do(func() {
		legacy, errLegacy = DecodeLegacy(legacyData)
	})
	if errLegacy != nil {
		return nil, errLegacy
	}
	return legacy[typeName], nil
}

// DecodeLegacy decodes a legacy record in the format the generator writes.
func DecodeLegacy(data []byte) (map[string][]Facts, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "opening the legacy identity record")
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the legacy identity record")
	}
	var out map[string][]Facts
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding the legacy identity record")
	}
	return out, nil
}
