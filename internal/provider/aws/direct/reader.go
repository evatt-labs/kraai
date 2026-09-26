package direct

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Reader is a compiled override: everything a client needs to read one
// type, resolved against the model, so nothing is looked up at run time.
type Reader struct {
	Type        string
	Protocol    string
	SigningName string
	// Host is the endpoint's host, with {region} standing for the client's
	// region in a regional one; SigningRegion, when set, is the region a
	// global endpoint is signed for.
	Host, SigningRegion string
	// Target is the X-Amz-Target header of an awsJson protocol.
	Target string
	// Method and URI are the HTTP binding of a restJson1 operation.
	Method, URI string
	// Action and Version are the form parameters of a query protocol, and
	// Wrapper the element awsQuery wraps its output in.
	Action, Version, Wrapper string
	Identifier               []Binding
	// Input is every fixed input, each with its Value.
	Input []Binding
	// Absent is every condition under which a returned instance is gone.
	Absent []Condition
	// AbsentErrors is every error code that means the instance is gone.
	AbsentErrors []string
	// Probe lists identifiers that must read as absent, for the harness.
	Probe *Lister
	// Also is the further calls whose properties are merged into a read.
	Also []Reader
	// Capture reads, keyed by Property, the values Also calls may name.
	Capture []Field
	// Create, Update and Delete are the compiled mutations; see Override.
	Create *MutationCall
	Update []MutationCall
	Delete *MutationCall
	// LifecycleComplete is true when every property an update can change
	// has an update call: with Production and lifecycle evidence, the only
	// readers a mutation may use in place of Cloud Control.
	LifecycleComplete bool
	// Mutable is true when the type may be created, updated and deleted
	// directly; see LifecycleComplete.
	Mutable bool
	// When is the read properties, and their values, an Also call is made
	// for; see Call.When.
	When []Condition
	// Each is the list property an Also call is made once per element of;
	// see Call.Each.
	Each string
	// AbsentIDs are identifiers that must read as absent, for the harness.
	AbsentIDs []string
	// Complete is true when the override skips no property at any depth,
	// so a read carries everything Cloud Control's does.
	Complete bool
	// Production is true when the reader is Complete, has the single
	// identifier a Cloud Control read gives it, and its recorded evidence
	// shows parity with Cloud Control both on the instances it reads and on
	// those it must read as absent: the only readers a lookup may use in
	// place of Cloud Control.
	Production bool
	// PageToken is the wire path to the output's page token when the
	// operation is paginated: a response carrying one is incomplete.
	PageToken []string
	// Response is the path from the output to the resource.
	Response []Step
	Fields   []Field
	// List lists every instance, when the type's override names a list.
	List *Lister
}

// Lister is a compiled list operation.
type Lister struct {
	Operation string
	// Target is the X-Amz-Target header of an awsJson protocol.
	Target string
	// Method and URI are the HTTP binding of a restJson1 operation.
	Method, URI string
	// Input is every fixed input, each with its Value.
	Input []Binding
	// Token places the page token in a request.
	Token Binding
	// NextToken and Items are wire paths in the output: the next page's
	// token, and the list of items.
	NextToken, Items []string
	// Item is the wire member of each item carrying Property, the primary
	// identifier; empty when each item is the identifier itself.
	Item, Property string
}

// Condition is the values of one resource member that mean the instance
// is absent; Field reads the member, keyed by its own name.
type Condition struct {
	Field  Field
	Values []string
}

// Step is one member of a response path: its name on the wire and, when
// it is a list, that the list must hold exactly the one resource read.
type Step struct {
	Name string
	List bool
	// Item is the element each list item is wrapped in, under an XML
	// protocol; empty when the list is flattened.
	Item string
	// Where and Equals select, from a list, the one element whose member
	// Where equals Equals, with {Property} standing for that identifier
	// property's value: a selection, not a projection.
	Where, Equals string
}

// Match is a member of a list element and the value it must equal, with
// {Property} standing for that identifier property's value.
type Match struct {
	Member, Equals string
}

// selection matches a path step that selects one element of a list, such
// as Associations[SubnetId={SubnetId}].
var selection = regexp.MustCompile(`^([A-Za-z0-9]+)\[([A-Za-z0-9]+)=(.+)\]$`)

// Binding places one primary identifier property in the request.
type Binding struct {
	Property string
	Member   string
	// Location is label, query, header, form or body.
	Location string
	// Name is the query parameter, header or form key, when Location is
	// one.
	Name string
	// List sends the value as a list of one, for an input that filters by
	// a list of identifiers.
	List bool
	// JSONName is the member's jsonName trait, when it has one.
	JSONName string
	// Value is a fixed input's value. {Property} in it stands for that
	// identifier property's value.
	Value string
	// Structured is a fixed input's value when it is a list or map, sent
	// as the member's structure under an awsJson protocol.
	Structured any
}

// Field reads one property from the response.
type Field struct {
	Property string
	Member   string
	// Via is the path on the wire from the enclosing structure to the one
	// holding Member, for a property the API wraps, such as a list inside
	// a Quantity and Items structure. A list step reads Member from every
	// element, making the property the list of those values.
	Via []Step
	// Transform is applied to the value read; see Mapping.Transform.
	Transform string
	// Where keeps, of a list of structures, the elements matching every
	// Match; see Mapping.Where.
	Where []Match
	// Key reads one entry of the map Member names.
	Key string
	// Entries and Keyed are Mapping.Entries and Mapping.Keyed, Keyed by
	// wire member; TrueWhen is Mapping.TrueWhen.
	Entries, Keyed, TrueWhen []string
	// Unless is Mapping.Unless: the read properties, and their values, for
	// which the field is left unread.
	Unless []Condition
	// Root reads the member from the operation's whole output rather than
	// the resource, for a value carried beside it.
	Root bool
	// JSONName is the member's jsonName trait, when it has one.
	JSONName string
	// Kind is scalar, timestamp, structure, list or map. A structure's
	// Fields are its own; a list's are those of each structure element.
	Kind   string
	Fields []Field
	// XMLName, Item and Scalar read the field under an XML protocol: its
	// element, the element wrapping each list item (empty when the list is
	// flattened), and how to type a scalar, a list's scalar items or a
	// map's values: string, number, boolean or timestamp.
	XMLName, Item, Scalar string
}

// protocolPreference is every protocol this package supports, in the order
// a service's is chosen when it declares more than one.
var protocolPreference = []string{"awsJson1_0", "awsJson1_1", "restJson1", "restXml", "awsQuery", "ec2Query"}

// isXML reports whether protocol answers in XML.
func isXML(protocol string) bool {
	return protocol == "awsQuery" || protocol == "ec2Query" || protocol == "restXml"
}

// isQuery reports whether protocol sends its input as a form.
func isQuery(protocol string) bool { return protocol == "awsQuery" || protocol == "ec2Query" }

// isREST reports whether protocol binds its input and output to HTTP.
func isREST(protocol string) bool { return protocol == "restJson1" || protocol == "restXml" }

// isAWSJSON reports whether protocol is one of the awsJson protocols,
// whose specifications say nothing of jsonName.
func isAWSJSON(protocol string) bool { return strings.HasPrefix(protocol, "awsJson") }

type smithyShape struct {
	Type    string                     `json:"type"`
	Version string                     `json:"version"`
	Traits  map[string]json.RawMessage `json:"traits"`
	Members map[string]smithyMember    `json:"members"`
	Member  *smithyMember              `json:"member"`
	Value   *smithyMember              `json:"value"`
	Input   *smithyMember              `json:"input"`
	Output  *smithyMember              `json:"output"`
}

type smithyMember struct {
	Target string                     `json:"target"`
	Traits map[string]json.RawMessage `json:"traits"`
}

type smithyModel struct {
	Shapes map[string]smithyShape `json:"shapes"`
}
