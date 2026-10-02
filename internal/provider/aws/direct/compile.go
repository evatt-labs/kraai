package direct

import (
	"encoding/json"
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
	wait, err := compileWait(o)
	if err != nil {
		errs = append(errs, err)
	}
	r.Wait = wait
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
			// The property is the read's, or an earlier further call's.
			fields := slices.Concat(r.Fields, alsoFields(r))
			i := slices.IndexFunc(fields, func(f Field) bool { return f.Property == call.Each })
			switch {
			case i >= 0 && fields[i].Wrap != "":
				// The element's one property is the name wrapped.
				callCaptured[fields[i].Wrap] = true
			case i < 0 || fields[i].Kind != "list" && fields[i].Kind != "structure" || len(fields[i].Fields) == 0:
				errs = append(errs, fmt.Errorf("also %s is made for each %s, which is not a structure or list of structures the read maps", call.Operation, call.Each))
			default:
				for _, f := range fields[i].Fields {
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
	errs = append(errs, checkWrapped(files, lock, o, append(slices.Clone(r.Fields), alsoFields(r)...))...)
	for _, name := range sortedKeys(o.Read.Capture) {
		named := false
		for _, call := range o.Also {
			for _, value := range call.Input {
				named = named || slices.Contains(placeholders(value), name)
			}
		}
		for _, m := range mutations(o) {
			named = named || slices.ContainsFunc(templateRefs(m.Input), func(ref [3]string) bool { return ref[0] == name })
		}
		if !named {
			errs = append(errs, fmt.Errorf("capture %s is named by no further call or mutation", name))
		}
	}
	r.AbsentIDs = o.AbsentIDs
	for _, p := range o.CreateOnly {
		r.CreateOnly = append(r.CreateOnly, "/properties/"+p)
	}
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

// checkWrapped refuses a wrapped list whose elements have a property no
// call made for each element reads: the names alone would pass for the
// whole element.
func checkWrapped(files fs.FS, lock Lock, o Override, fields []Field) []error {
	var errs []error
	for _, f := range fields {
		if f.Wrap == "" {
			continue
		}
		var schema cfnSchema
		if raw, err := fs.ReadFile(files, lock.Schemas[o.Type].File); err != nil || json.Unmarshal(raw, &schema) != nil {
			return []error{fmt.Errorf("%s has no readable locked schema", o.Type)}
		}
		for _, name := range sortedKeys(schema.nested(schema.Properties[f.Property])) {
			read := schema.writeOnlyAt(f.Property+"."+name) || name == f.Wrap || slices.ContainsFunc(o.Also, func(c Call) bool {
				_, mapped := c.Properties[name]
				return c.Each == f.Property && mapped
			})
			if !read {
				errs = append(errs, fmt.Errorf("%s wraps names as %s, but no call made for each element reads its %s", f.Property, f.Wrap, name))
			}
		}
	}
	return errs
}
