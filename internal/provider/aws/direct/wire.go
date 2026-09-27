package direct

import (
	"errors"
	"fmt"
	"slices"
)

// wireable reports why f's read mapping cannot be run backwards, or nil
// when a CloudFormation value of it can be rewritten into its wire shape
// by renaming members alone.
func wireable(f Field) error {
	switch {
	case len(f.Via) > 0, len(f.Where) > 0, f.Key != "", len(f.Entries) > 0, len(f.Keyed) > 0, len(f.TrueWhen) > 0, len(f.Unless) > 0:
		return fmt.Errorf("%s is read through a selection or reshaping", f.Property)
	case f.Transform != "":
		return fmt.Errorf("%s is read through the %s transform", f.Property, f.Transform)
	case f.Kind == "timestamp":
		// CloudFormation writes a timestamp as text; awsJson sends seconds.
		return fmt.Errorf("%s is a timestamp", f.Property)
	}
	var errs []error
	for _, child := range f.Fields {
		errs = append(errs, wireable(child))
	}
	return errors.Join(errs...)
}

// wire rewrites a CloudFormation value of f into the shape f is read from:
// each structure's properties under their wire member names. A property
// the read does not map is an error, never dropped.
func wire(f Field, v any) (any, error) {
	switch f.Kind {
	case "structure":
		return wireStructure(f, v)
	case "list":
		if len(f.Fields) == 0 {
			return v, nil
		}
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s is not a list", f.Property)
		}
		out := make([]any, len(items))
		for i, item := range items {
			w, err := wireStructure(f, item)
			if err != nil {
				return nil, err
			}
			out[i] = w
		}
		return out, nil
	}
	return v, nil
}

// wireStructure rewrites one structure of f's fields.
func wireStructure(f Field, v any) (any, error) {
	props, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", f.Property)
	}
	out := make(map[string]any, len(props))
	for _, name := range sortedKeys(props) {
		i := slices.IndexFunc(f.Fields, func(c Field) bool { return c.Property == name })
		if i < 0 {
			return nil, fmt.Errorf("%s.%s has no wire member", f.Property, name)
		}
		w, err := wire(f.Fields[i], props[name])
		if err != nil {
			return nil, fmt.Errorf("%s.%w", f.Property, err)
		}
		out[f.Fields[i].Member] = w
	}
	return out, nil
}
