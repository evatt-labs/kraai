package direct

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
)

// Compile checks every override against its locked model and schema and
// returns the readers, sorted by type. Every problem is reported, not only
// the first.
func Compile() ([]Reader, error) { return compileAll(files) }

func compileAll(files fs.FS) ([]Reader, error) {
	if err := verify(files); err != nil {
		return nil, err
	}
	lock, err := loadLock(files)
	if err != nil {
		return nil, err
	}
	all, err := overrides(files)
	if err != nil {
		return nil, err
	}
	var readers []Reader
	var problems []error
	for _, o := range all {
		r, errs := compileOne(files, lock, o)
		for _, e := range errs {
			problems = append(problems, fmt.Errorf("%s: %w", o.Type, e))
		}
		if len(errs) == 0 {
			readers = append(readers, r)
		}
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return readers, nil
}

func compileOne(files fs.FS, lock Lock, o Override) (Reader, []error) {
	r, errs := compileCall(files, lock, o, nil, nil, "")
	captured := map[string]bool{}
	for name := range o.Read.Capture {
		captured[name] = true
	}
	for i, call := range o.Also {
		only := map[string]bool{}
		for name := range call.Properties {
			only[name] = true
		}
		sub := Override{
			Type: o.Type,
			Read: Read{Model: o.Read.Model, Operation: call.Operation, Identifier: call.Identifier,
				Response: call.Response, Input: call.Input, AbsentErrors: call.AbsentErrors},
			Properties: call.Properties,
		}
		callCaptured := captured
		if call.Each != "" {
			callCaptured = maps.Clone(captured)
			i := slices.IndexFunc(r.Fields, func(f Field) bool { return f.Property == call.Each })
			if i < 0 || r.Fields[i].Kind != "list" && r.Fields[i].Kind != "structure" || len(r.Fields[i].Fields) == 0 {
				errs = append(errs, fmt.Errorf("also %s is made for each %s, which is not a structure or list of structures the read maps", call.Operation, call.Each))
			} else {
				for _, f := range r.Fields[i].Fields {
					if f.Kind == "scalar" {
						callCaptured[f.Property] = true
					}
				}
			}
		}
		also, alsoErrs := compileCall(files, lock, sub, only, callCaptured, call.Each)
		also.Each = call.Each
		for _, e := range alsoErrs {
			errs = append(errs, fmt.Errorf("also[%d] %s: %w", i, call.Operation, e))
		}
		if len(call.Properties) == 0 {
			errs = append(errs, fmt.Errorf("also[%d] %s maps no property", i, call.Operation))
		}
		for _, property := range sortedKeys(call.When) {
			j := slices.IndexFunc(r.Fields, func(f Field) bool { return f.Property == property })
			switch {
			case j < 0 || r.Fields[j].Kind != "scalar":
				errs = append(errs, fmt.Errorf("also[%d] %s is made when %s, which is not a scalar property of the read", i, call.Operation, property))
			case len(call.When[property]) == 0:
				errs = append(errs, fmt.Errorf("also[%d] %s is made when %s has no value", i, call.Operation, property))
			default:
				also.When = append(also.When, Condition{Field: Field{Property: property}, Values: call.When[property]})
			}
		}
		r.Also = append(r.Also, also)
	}
	for _, name := range sortedKeys(o.Read.Capture) {
		named := false
		for _, call := range o.Also {
			for _, value := range call.Input {
				named = named || slices.Contains(placeholders(value), name)
			}
		}
		if !named {
			errs = append(errs, fmt.Errorf("capture %s is named by no further call", name))
		}
	}
	r.AbsentIDs = o.AbsentIDs
	if o.Create != nil || o.Update != nil || o.Delete != nil {
		errs = append(errs, compileMutations(files, lock, o, &r)...)
	}
	return r, errs
}

// skipsAny reports whether an override skips any property, at any depth.
func skipsAny(skip map[string]string, mapped map[string]Mapping) bool {
	if len(skip) > 0 {
		return true
	}
	for _, m := range mapped {
		if skipsAny(m.Skip, m.Properties) {
			return true
		}
	}
	return false
}
