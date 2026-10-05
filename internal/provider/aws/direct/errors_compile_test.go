package direct

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// errorModel declares NoSuchEntityException, sent as NoSuchEntity under a
// query protocol, and ConflictException, which no awsQueryError renames.
func errorModel() *smithyModel {
	errTrait := json.RawMessage(`"client"`)
	return &smithyModel{Shapes: map[string]smithyShape{
		"com.example#NoSuchEntityException": {Type: "structure", Traits: map[string]json.RawMessage{
			"smithy.api#error":            errTrait,
			"aws.protocols#awsQueryError": json.RawMessage(`{"code": "NoSuchEntity", "httpResponseCode": 404}`),
		}},
		"com.example#ConflictException": {Type: "structure", Traits: map[string]json.RawMessage{"smithy.api#error": errTrait}},
		"com.example#Widget":            {Type: "structure"},
	}}
}

func TestCheckErrorCodes(t *testing.T) {
	cases := []struct {
		name           string
		protocol       string
		o              Override
		refused        string
		read, deletion []string
	}{
		{name: "the shape name under JSON", protocol: "awsJson1_1",
			o: Override{Read: Read{AbsentErrors: []string{"NoSuchEntityException"}}}},
		{name: "the awsQueryError code under awsQuery", protocol: "awsQuery",
			o: Override{Read: Read{AbsentErrors: []string{"NoSuchEntity"}}}},
		{name: "the shape name under awsQuery, which the wire never carries", protocol: "awsQuery",
			o:       Override{Read: Read{AbsentErrors: []string{"NoSuchEntityException"}}},
			refused: "read absentErrors names NoSuchEntityException, which no error shape"},
		{name: "a shape with no awsQueryError under awsQuery", protocol: "awsQuery",
			o: Override{Delete: &Mutation{RetryErrors: []string{"ConflictException: still in use"}}}},
		{name: "a misspelled code", protocol: "awsJson1_1",
			o:       Override{Delete: &Mutation{AbsentErrors: []string{"NoSuchEntityExeption"}}},
			refused: "delete absentErrors names NoSuchEntityExeption"},
		{name: "a misspelled retry code before its text", protocol: "awsJson1_1",
			o:       Override{Delete: &Mutation{RetryErrors: []string{"ConflictExeption: still in use"}}},
			refused: "delete retryErrors names ConflictExeption"},
		{name: "a structure that is not an error", protocol: "awsJson1_1",
			o:       Override{Read: Read{AbsentErrors: []string{"Widget"}}},
			refused: "read absentErrors names Widget"},
		{name: "an undeclared code listed with why", protocol: "ec2Query",
			o: Override{
				Read:             Read{AbsentErrors: []string{"InvalidVpcID.NotFound"}},
				Delete:           &Mutation{AbsentErrors: []string{"InvalidVpcID.NotFound", "Gone"}},
				Update:           []UpdateCall{{Before: &Mutation{AbsentErrors: []string{"Gateway.NotAttached"}}}},
				UndeclaredErrors: map[string]string{"InvalidVpcID.NotFound": "EC2 declares no errors.", "Gone": "EC2 declares no errors.", "Gateway.NotAttached": "EC2 declares no errors."},
			},
			read: []string{"InvalidVpcID.NotFound"}, deletion: []string{"Gone", "InvalidVpcID.NotFound"}},
		{name: "a listing no call names", protocol: "ec2Query",
			o:       Override{UndeclaredErrors: map[string]string{"Gone": "EC2 declares no errors."}},
			refused: "undeclaredErrors lists Gone, which no absentErrors or retryErrors names"},
		{name: "a listing of a declared code", protocol: "awsJson1_1",
			o:       Override{Read: Read{AbsentErrors: []string{"ConflictException"}}, UndeclaredErrors: map[string]string{"ConflictException": "x"}},
			refused: "undeclaredErrors lists ConflictException, which the model declares"},
		{name: "a listing without why", protocol: "ec2Query",
			o:       Override{Read: Read{AbsentErrors: []string{"Gone"}}, UndeclaredErrors: map[string]string{"Gone": " "}},
			refused: "undeclaredErrors lists Gone without why", read: []string{"Gone"}},
		{name: "absentErrors on a create, which nothing reads", protocol: "awsJson1_1",
			o:       Override{Create: &Create{Mutation: Mutation{AbsentErrors: []string{"ConflictException"}}}},
			refused: "create names absentErrors"},
		{name: "absentErrors on an update's own call", protocol: "awsJson1_1",
			o:       Override{Update: []UpdateCall{{Mutation: Mutation{Operation: "UpdateWidget", AbsentErrors: []string{"ConflictException"}}}}},
			refused: "update UpdateWidget names absentErrors"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			read, deletion, errs := checkErrorCodes(errorModel(), c.protocol, c.o)
			var msgs []string
			for _, e := range errs {
				msgs = append(msgs, e.Error())
			}
			got := strings.Join(msgs, "; ")
			if c.refused == "" && got != "" || c.refused != "" && !strings.Contains(got, c.refused) {
				t.Fatalf("errors = %q, want %q", got, c.refused)
			}
			if !slices.Equal(read, c.read) || !slices.Equal(deletion, c.deletion) {
				t.Fatalf("undeclared read %v, delete %v; want %v, %v", read, deletion, c.read, c.deletion)
			}
		})
	}
}
