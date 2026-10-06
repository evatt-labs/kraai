package direct

import (
	"encoding/json"
	"io/fs"
)

// decodedFS is a set of checked-in files that keeps each model and schema
// it has decoded, for one compilation: every override decodes its model
// and schema several times, and many share a model.
type decodedFS struct {
	fs.FS
	models  map[string]smithyModel
	schemas map[string]cfnSchema
	// ruleSets is each model's parsed endpoint rule set, which every call
	// through the model resolves against; S3's is over 600 KB.
	ruleSets map[string]parsedRuleSet
}

type parsedRuleSet struct {
	rs  *ruleSet
	err error
}

func withDecoded(files fs.FS) *decodedFS {
	return &decodedFS{FS: files, models: map[string]smithyModel{}, schemas: map[string]cfnSchema{}, ruleSets: map[string]parsedRuleSet{}}
}

// loadModel decodes the model in file, once per compilation when files is
// a decodedFS. Compilation never writes into a model it is given.
func loadModel(files fs.FS, file string) (smithyModel, error) {
	cache, cached := files.(*decodedFS)
	if cached {
		if m, ok := cache.models[file]; ok {
			return m, nil
		}
	}
	var model smithyModel
	raw, err := fs.ReadFile(files, file)
	if err == nil {
		err = json.Unmarshal(raw, &model)
	}
	if err != nil {
		return smithyModel{}, err
	}
	if cached {
		cache.models[file] = model
	}
	return model, nil
}

// loadSchema decodes the schema in file as loadModel does a model. The
// schema returned is a copy whose elsewhere is its caller's to set.
func loadSchema(files fs.FS, file string) (cfnSchema, error) {
	cache, cached := files.(*decodedFS)
	if cached {
		if s, ok := cache.schemas[file]; ok {
			return s, nil
		}
	}
	var schema cfnSchema
	raw, err := fs.ReadFile(files, file)
	if err == nil {
		err = json.Unmarshal(raw, &schema)
	}
	if err != nil {
		return cfnSchema{}, err
	}
	if cached {
		cache.schemas[file] = schema
	}
	return schema, nil
}

// loadRuleSet parses raw, the endpoint rule set of the model in file, once
// per compilation when files is a decodedFS.
func loadRuleSet(files fs.FS, file string, raw json.RawMessage) (*ruleSet, error) {
	cache, cached := files.(*decodedFS)
	if cached {
		if p, ok := cache.ruleSets[file]; ok {
			return p.rs, p.err
		}
	}
	rs, err := parseRuleSet(raw)
	if cached {
		cache.ruleSets[file] = parsedRuleSet{rs, err}
	}
	return rs, err
}
