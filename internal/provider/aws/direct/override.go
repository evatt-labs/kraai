package direct

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// Override describes how one CloudFormation type is read through its
// service's own API. Every property is either mapped or skipped with a
// reason, at every depth: nothing is matched by name when code is
// generated from it.
type Override struct {
	// Type is the CloudFormation type name, such as AWS::XRay::Group.
	Type string `yaml:"type"`
	Read Read   `yaml:"read"`
	// List, when set, names the operation that lists every instance.
	List *List `yaml:"list,omitempty"`
	// Probe lists identifiers Cloud Control reads as absent although the
	// service still describes them, such as deregistered revisions, for
	// the read-parity harness to prove the direct read agrees.
	Probe *List `yaml:"probe,omitempty"`
	// Properties maps each readable CloudFormation property to where the
	// response carries it.
	Properties map[string]Mapping `yaml:"properties,omitempty"`
	// Skip names each property the read does not carry, and why.
	Skip map[string]string `yaml:"skip,omitempty"`
	// Also names further calls whose responses carry properties the read
	// does not, each addressed by the same identifier.
	Also []Call `yaml:"also,omitempty"`
	// AbsentIDs are identifiers of instances that do not exist, for the
	// read-parity harness to prove the direct read agrees on absence of a
	// type whose service never describes a gone instance. {account} and
	// {region} stand for the account and region the harness runs in.
	AbsentIDs []string `yaml:"absentIds,omitempty"`
}

// Call is one further call of a read: an operation of the same model,
// addressed and walked as Read is, and the properties its response
// carries.
type Call struct {
	Operation  string             `yaml:"operation"`
	Identifier map[string]string  `yaml:"identifier,omitempty"`
	Response   string             `yaml:"response,omitempty"`
	Input      map[string]any     `yaml:"input,omitempty"`
	Properties map[string]Mapping `yaml:"properties"`
	// When makes the call only for an instance whose read property has one
	// of the values given, for an operation the service refuses on others,
	// such as index policies on a log group of another class.
	When map[string][]string `yaml:"when,omitempty"`
}

// Read names the operation that reads one instance and how to call it.
type Read struct {
	// Model is the model file's path under models/ in
	// github.com/aws/api-models-aws.
	Model string `yaml:"model"`
	// Operation is the operation's name within that model.
	Operation string `yaml:"operation"`
	// Identifier binds each primary identifier property to the input
	// member that carries it.
	Identifier map[string]string `yaml:"identifier,omitempty"`
	// Response is the dotted member path from the operation's output to
	// the structure holding the resource; empty when the output is it.
	Response string `yaml:"response,omitempty"`
	// Input fixes input members to a value on every read, such as asking
	// for tags the operation otherwise leaves out. A string sent to a list
	// member is sent as a list of one. A list or map is sent as the
	// member's structure, such as EC2's Filters, under a query or awsJson
	// protocol. A string may name an identifier property as {Property},
	// replaced by its value on every call.
	Input map[string]any `yaml:"input,omitempty"`
	// Absent names members of the resource structure and the values that
	// mean the instance is gone although the service still returns it,
	// such as a status of INACTIVE.
	Absent map[string][]string `yaml:"absent,omitempty"`
	// AbsentErrors names the error codes the read answers for an instance
	// that does not exist, such as InvalidGroup.NotFound, which the reader
	// reports as absence rather than as an error.
	AbsentErrors []string `yaml:"absentErrors,omitempty"`
	// Capture names string members of the resource structure, as dotted
	// paths, that a further call's input may use as {Name}: a value only
	// the read's own response carries, such as the ARN a tag call takes.
	Capture map[string]string `yaml:"capture,omitempty"`
}

// List names the operation that lists every instance of a type, for a type
// whose Cloud Control list is known to omit some. Pagination is read from
// the operation's paginated trait, never written here.
type List struct {
	Operation string `yaml:"operation"`
	// Item is the member of each listed item carrying the primary
	// identifier; it must be the member the read binds it to. Omitted when
	// each listed item is the identifier itself, a list of strings.
	Item string `yaml:"item,omitempty"`
	// Input fixes input members to a value on every call, such as a filter
	// that would otherwise default to excluding what kraai created.
	Input map[string]string `yaml:"input,omitempty"`
}

// Mapping is where one property's value is in the response: a member of
// the enclosing structure and, for a structure or a list of structures,
// how each nested property maps. A member path whose last step follows a
// map reads that key's value, such as Attributes.DelaySeconds; a member
// of {Property} reads the identifier property's own value, for one the
// response does not repeat. A top-level property may instead name a
// member of the operation's whole output with a leading "$.", for a value
// the output carries beside the resource, such as its tags.
type Mapping struct {
	Member     string             `yaml:"member"`
	Properties map[string]Mapping `yaml:"properties,omitempty"`
	Skip       map[string]string  `yaml:"skip,omitempty"`
	// Transform names a function applied to the value read: arnResource,
	// an ARN's resource part; or json, number or boolean, parsing a string
	// the service returns for a property the schema types otherwise.
	Transform string `yaml:"transform,omitempty"`
	// Entries reads a map as a list of structures, each holding one entry
	// under the two property names given, key first, such as tags returned
	// as a map for a schema's [{Key, Value}].
	Entries []string `yaml:"entries,omitempty"`
	// Keyed reads a list of structures as a map, keyed by the first member
	// named and valued by the second, such as parameters returned as
	// [{ParameterName, ParameterValue}] for a schema's object.
	Keyed []string `yaml:"keyed,omitempty"`
	// Where keeps, of a list of structures, only the elements whose member
	// equals the value, such as the ingress rules of a list holding both
	// directions. {Property} stands for an identifier property's value.
	Where map[string]string `yaml:"where,omitempty"`
}

// UnmarshalYAML accepts a bare member name for a mapping with no nested
// properties.
func (m *Mapping) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		m.Member = node.Value
		return nil
	}
	type plain Mapping
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*m = Mapping(p)
	return nil
}

// MarshalYAML writes a mapping with no nested properties as its bare
// member name, the form it is reviewed in.
func (m Mapping) MarshalYAML() (any, error) {
	if len(m.Properties) == 0 && len(m.Skip) == 0 && m.Transform == "" && len(m.Where) == 0 && len(m.Entries) == 0 && len(m.Keyed) == 0 {
		return m.Member, nil
	}
	type plain Mapping
	return plain(m), nil
}

// String is for error messages.
func (m Mapping) String() string { return fmt.Sprintf("member %s", m.Member) }
