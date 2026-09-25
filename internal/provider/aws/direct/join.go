package direct

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing/fstest"
)

// JoinModel is one Smithy model offered to Join: its path under models/ in
// github.com/aws/api-models-aws, and its content.
type JoinModel struct {
	Path string
	Raw  []byte
}

// Joined is Join's verdict on one CloudFormation type: an override that
// compiles, or the reasons there is none.
type Joined struct {
	Type     string
	Protocol string
	Override *Override
	Reasons  []string
}

// generatedTagsReason is the skip reason Join writes for a Tags property
// no member of the resource structure matches, which is all propose can
// know: where the service returns tags, if anywhere, is not its to say.
const generatedTagsReason = "no member of the read's resource structure matches Tags by name and type; not mapped mechanically"

// Join drafts a read override for every schema whose type it can join to a
// model mechanically, with no person in the loop: the model whose
// cloudFormationName is the type's service, the one Get or Describe
// operation the type's read handler is permitted to call, and a mapping
// that leaves no property unaccounted for. A type is returned with an
// override only when that override compiles; otherwise with the reasons.
func Join(models []JoinModel, schemas [][]byte) ([]Joined, error) {
	byPrefix := map[string][]*joinModel{}
	for _, m := range models {
		jm, err := loadJoinModel(m)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Path, err)
		}
		if jm.signingName != "" {
			byPrefix[jm.signingName] = append(byPrefix[jm.signingName], jm)
		}
	}

	var out []Joined
	for _, raw := range schemas {
		var schema joinSchema
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, err
		}
		out = append(out, joinOne(byPrefix, schema, raw))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out, nil
}

type joinModel struct {
	path  string
	raw   []byte
	model smithyModel
	// cfnName, sdkID and arnNamespace name the service; signingName is
	// also the prefix of its IAM actions.
	cfnName, sdkID, arnNamespace, signingName string
	protocol                                  string
	// actions maps each operation's IAM action name to the operation.
	actions map[string]string
}

// joinSchema is cfnSchema plus what only Join reads.
type joinSchema struct {
	cfnSchema
	TypeName string `json:"typeName"`
	Handlers map[string]struct {
		Permissions []string `json:"permissions"`
	} `json:"handlers"`
}

func loadJoinModel(m JoinModel) (*joinModel, error) {
	jm := &joinModel{path: m.Path, raw: m.Raw, actions: map[string]string{}}
	if err := json.Unmarshal(m.Raw, &jm.model); err != nil {
		return nil, err
	}
	for id, s := range jm.model.Shapes {
		switch s.Type {
		case "service":
			var api struct{ CloudFormationName, SdkID, ArnNamespace string }
			var sigv4 struct{ Name string }
			_ = json.Unmarshal(s.Traits["aws.api#service"], &api)
			_ = json.Unmarshal(s.Traits["aws.auth#sigv4"], &sigv4)
			jm.cfnName, jm.sdkID, jm.arnNamespace, jm.signingName = api.CloudFormationName, api.SdkID, api.ArnNamespace, sigv4.Name
			for trait := range s.Traits {
				// awsQueryCompatible marks a JSON service that still
				// answers query-style errors; it is not a protocol.
				if p, ok := strings.CutPrefix(trait, "aws.protocols#"); ok && p != "awsQueryCompatible" {
					jm.protocol = p
				}
			}
		case "operation":
			name := id[strings.Index(id, "#")+1:]
			action := name
			var iam string
			if json.Unmarshal(s.Traits["aws.iam#iamAction"], &struct{ Name *string }{&iam}) == nil && iam != "" {
				action = iam
			}
			jm.actions[action] = name
		}
	}
	return jm, nil
}

func joinOne(byPrefix map[string][]*joinModel, schema joinSchema, schemaRaw []byte) Joined {
	j := Joined{Type: schema.TypeName}
	parts := strings.Split(schema.TypeName, "::")
	if len(parts) != 3 {
		return j.because("type name is not AWS::<service>::<resource>")
	}
	if len(schema.PrimaryIdentifier) != 1 {
		return j.because("composite primary identifier")
	}

	// Every Get or Describe operation the read handler may call, in every
	// model its IAM prefix names.
	type candidate struct {
		model *joinModel
		op    string
	}
	var found []candidate
	seen := map[candidate]bool{}
	for _, permission := range schema.Handlers["read"].Permissions {
		prefix, action, ok := strings.Cut(permission, ":")
		if !ok {
			continue
		}
		for _, jm := range byPrefix[prefix] {
			op, ok := jm.actions[action]
			if !ok || !strings.HasPrefix(op, "Get") && !strings.HasPrefix(op, "Describe") {
				continue
			}
			if c := (candidate{jm, op}); !seen[c] {
				seen[c] = true
				found = append(found, c)
			}
		}
	}
	// Models sharing a signing name (RDS, Neptune and DocumentDB) are told
	// apart by which one names the type's own service, trying the most
	// specific name first: they often share an ARN namespace too.
	if len(found) > 1 {
		for _, names := range []func(*joinModel) bool{
			func(m *joinModel) bool { return m.cfnName == parts[1] },
			func(m *joinModel) bool { return normalize(m.sdkID) == normalize(parts[1]) },
			func(m *joinModel) bool { return m.arnNamespace == strings.ToLower(parts[1]) },
		} {
			var own []candidate
			for _, c := range found {
				if names(c.model) {
					own = append(own, c)
				}
			}
			if len(own) > 0 {
				found = own
				break
			}
		}
	}
	switch len(found) {
	case 0:
		return j.because("read handler calls no Get or Describe operation of any model")
	case 1:
	default:
		return j.because("read handler calls several Get or Describe operations")
	}
	jm, op := found[0].model, found[0].op
	j.Protocol = jm.protocol
	reads := []string{op}

	o, err := proposeWith(&jm.model, &schema.cfnSchema, Override{
		Type: schema.TypeName,
		Read: Read{Model: jm.path, Operation: reads[0]},
	})
	if err != nil {
		return j.because(err.Error())
	}
	if reason, ok := o.Skip["Tags"]; ok && strings.HasPrefix(reason, "TODO") {
		o.Skip["Tags"] = generatedTagsReason
	}
	if todo := todos(o.Properties, o.Skip, ""); len(todo) > 0 {
		return j.because("unmatched properties: " + strings.Join(todo, ", "))
	}

	files := fstest.MapFS{"model.json": {Data: jm.raw}, "schema.json": {Data: schemaRaw}}
	lock := Lock{
		Models:  map[string]LockedFile{jm.path: {File: "model.json"}},
		Schemas: map[string]LockedFile{schema.TypeName: {File: "schema.json"}},
	}
	if _, errs := compileOne(files, lock, o); len(errs) > 0 {
		for _, e := range errs {
			j.Reasons = append(j.Reasons, "does not compile: "+e.Error())
		}
		return j
	}
	j.Override = &o
	return j
}

func (j Joined) because(reason string) Joined {
	j.Reasons = append(j.Reasons, reason)
	return j
}

// todos lists every property still skipped with a TODO reason, at any
// depth, by dotted path.
func todos(mapped map[string]Mapping, skipped map[string]string, at string) []string {
	var out []string
	for _, name := range sortedKeys(skipped) {
		if strings.HasPrefix(skipped[name], "TODO") {
			out = append(out, at+name)
		}
	}
	for _, name := range sortedKeys(mapped) {
		m := mapped[name]
		out = append(out, todos(m.Properties, m.Skip, at+name+".")...)
	}
	return out
}
