package direct

import (
	"slices"
	"sort"
)

// restNamedServices are the services whose IAM actions are not named after
// their API operations: API Gateway grants by HTTP verb, and S3's actions
// diverge from its operations' names. Their direct calls are covered by
// the permissions the type's own schema publishes for the same APIs.
var restNamedServices = []string{"apigateway", "s3"}

// IAMActions is every IAM action the direct calls for typeName make,
// besides what its schema's handlers publish: each read, further read,
// create, update and delete operation its override names, as
// <service>:<Operation>. Empty for a type with no direct reader, or one
// of restNamedServices.
func IAMActions(typeName string) ([]string, error) {
	r, ok := readers[typeName]
	if !ok || slices.Contains(restNamedServices, r.SigningName) {
		return nil, nil
	}
	all, err := Overrides()
	if err != nil {
		return nil, err
	}
	for _, o := range all {
		if o.Type != typeName {
			continue
		}
		var actions []string
		for _, op := range operations(o) {
			if op != "" && !slices.Contains(actions, r.SigningName+":"+op) {
				actions = append(actions, r.SigningName+":"+op)
			}
		}
		sort.Strings(actions)
		return actions, nil
	}
	return nil, nil
}

// operations is every operation an override names.
func operations(o Override) []string {
	ops := []string{o.Read.Operation}
	for _, also := range o.Also {
		ops = append(ops, also.Operation)
	}
	if o.Create != nil {
		ops = append(ops, o.Create.Operation)
	}
	if o.Delete != nil {
		ops = append(ops, o.Delete.Operation)
	}
	for _, u := range o.Update {
		ops = append(ops, u.Operation)
		if u.Tags != nil {
			ops = append(ops, u.Tags.Add.Operation, u.Tags.Remove.Operation)
		}
		if u.List != nil {
			ops = append(ops, u.List.Add.Operation)
			if u.List.Remove != nil {
				ops = append(ops, u.List.Remove.Operation)
			}
			if u.List.Change != nil {
				ops = append(ops, u.List.Change.Operation)
			}
			for _, c := range u.List.Changes {
				ops = append(ops, c.Operation)
			}
		}
		if u.Before != nil {
			ops = append(ops, u.Before.Operation)
		}
		if u.WhenEmpty != nil {
			ops = append(ops, u.WhenEmpty.Operation)
		}
	}
	return ops
}
