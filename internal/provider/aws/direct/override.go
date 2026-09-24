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
	// Properties maps each readable CloudFormation property to where the
	// response carries it.
	Properties map[string]Mapping `yaml:"properties,omitempty"`
	// Skip names each property the read does not carry, and why.
	Skip map[string]string `yaml:"skip,omitempty"`
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
}

// Mapping is where one property's value is in the response: a member of
// the enclosing structure and, for a structure or a list of structures,
// how each nested property maps.
type Mapping struct {
	Member     string             `yaml:"member"`
	Properties map[string]Mapping `yaml:"properties,omitempty"`
	Skip       map[string]string  `yaml:"skip,omitempty"`
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
	if len(m.Properties) == 0 && len(m.Skip) == 0 {
		return m.Member, nil
	}
	type plain Mapping
	return plain(m), nil
}

// String is for error messages.
func (m Mapping) String() string { return fmt.Sprintf("member %s", m.Member) }
