package direct

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// serveLambda answers every request with handle's status and body, and the
// error type header Lambda sets on a refusal.
func serveLambda(t *testing.T, handle func(r *http.Request) (status int, errType string, body any)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, errType, body := handle(r)
		if errType != "" {
			w.Header().Set("X-Amzn-Errortype", errType)
		}
		out, _ := json.Marshal(body)
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// A policy is one JSON document in a string, holding every statement of the
// function.
func policyOf(statements ...map[string]any) map[string]any {
	doc, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Id": "default", "Statement": statements})
	return map[string]any{"Policy": string(doc), "RevisionId": "r"}
}

const urlFunctionARN = "arn:aws:lambda:us-east-1:1:function:fn"

// The target and qualifier read as parts of the identifier ARN, split on
// colons, and a qualifier an unqualified function lacks reads as absent,
// not as an empty string. The URL is read with the identifier as the one
// escaped label.
func TestReadURLSplitsTheARN(t *testing.T) {
	for id, want := range map[string]map[string]any{
		urlFunctionARN:           {"TargetFunctionArn": "fn"},
		urlFunctionARN + ":live": {"TargetFunctionArn": "fn", "Qualifier": "live"},
	} {
		var path string
		client := serveLambda(t, func(r *http.Request) (int, string, any) {
			path = r.Method + " " + r.URL.EscapedPath()
			return 200, "", map[string]any{"FunctionArn": id, "FunctionUrl": "https://u/", "AuthType": "AWS_IAM", "InvokeMode": "BUFFERED"}
		})
		got, err := client.ReadByID(context.Background(), "AWS::Lambda::Url", id)
		if err != nil {
			t.Fatal(err)
		}
		if want := "GET /2021-10-31/functions/" + strings.ReplaceAll(id, ":", "%3A") + "/url"; path != want {
			t.Errorf("request = %q, want %q", path, want)
		}
		for _, p := range []string{"TargetFunctionArn", "Qualifier"} {
			if got[p] != want[p] {
				t.Errorf("%s: %s = %v, want %v", id, p, got[p], want[p])
			}
		}
		if _, has := got["Qualifier"]; has != (want["Qualifier"] != nil) {
			t.Errorf("%s: Qualifier present = %v", id, has)
		}
	}
}

func TestReadURLAbsence(t *testing.T) {
	client := serveLambda(t, func(*http.Request) (int, string, any) {
		return 404, "ResourceNotFoundException", map[string]any{"Message": "gone"}
	})
	if _, err := client.ReadByID(context.Background(), "AWS::Lambda::Url", urlFunctionARN); !errors.Is(err, ErrAbsent) {
		t.Fatalf("read = %v, want absent", err)
	}
}

func TestTransformArnPart(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:1:function:fn:live"
	for _, c := range []struct {
		name string
		in   any
		want any
		ok   bool
	}{
		{"arnPart:6", arn, "fn", true},
		{"arnPart:7", arn, "live", true},
		{"arnPart:7", urlFunctionARN, nil, false},
		{"arnPart:7", "arn:aws:lambda:us-east-1:1:function:fn:", nil, false},
		{"arnPart:0", arn, "arn", true},
		{"arnPart:6", 42, 42, true},
	} {
		if got, ok := transform(c.name, c.in); got != c.want || ok != c.ok {
			t.Errorf("transform(%s, %v) = %v, %v; want %v, %v", c.name, c.in, got, ok, c.want, c.ok)
		}
	}
	for _, name := range []string{"arnPart:", "arnPart:x", "arnPart:-1", "arnPart:+1", "arnPart:1000", "arnPart"} {
		if n, ok := arnPartIndex(name); ok {
			t.Errorf("arnPartIndex(%q) = %d, want no index", name, n)
		}
	}
}

// A statement of the function's policy that is not this one is no
// permission of this one: the instance is absent, and so is a function with
// no policy at all. The identifier selects the statement, and the
// principal is a service or an AWS principal as the statement names it.
func TestReadPermission(t *testing.T) {
	other := map[string]any{"Sid": "other", "Action": "lambda:InvokeFunction", "Principal": map[string]any{"Service": "s3.amazonaws.com"},
		"Condition": map[string]any{"ArnLike": map[string]any{"AWS:SourceArn": "arn:aws:s3:::b"}, "Bool": map[string]any{"lambda:InvokedViaFunctionUrl": "true"}}}
	mine := map[string]any{"Sid": "mine", "Action": "lambda:InvokeFunctionUrl", "Principal": map[string]any{"AWS": "arn:aws:iam::1:root"}}
	var paths []string
	statements := []map[string]any{other, mine}
	client := serveLambda(t, func(r *http.Request) (int, string, any) {
		paths = append(paths, r.Method+" "+r.URL.EscapedPath())
		if len(statements) == 0 {
			return 404, "ResourceNotFoundException", map[string]any{"Message": "none"}
		}
		return 200, "", policyOf(statements...)
	})
	ctx := context.Background()
	got, err := client.ReadByID(ctx, "AWS::Lambda::Permission", "fn:live|other")
	want := map[string]any{"FunctionName": "fn:live", "Id": "other", "Action": "lambda:InvokeFunction", "Principal": "s3.amazonaws.com",
		"SourceArn": "arn:aws:s3:::b", "InvokedViaFunctionUrl": true}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("read = %v, %v\nwant %v", got, err, want)
	}
	if paths[0] != "GET /2015-03-31/functions/fn%3Alive/policy" {
		t.Fatalf("request = %q", paths[0])
	}
	if got, err := client.ReadByID(ctx, "AWS::Lambda::Permission", "fn|mine"); err != nil || got["Principal"] != "arn:aws:iam::1:root" {
		t.Fatalf("an AWS principal = %v, %v", got, err)
	}
	if _, err := client.ReadByID(ctx, "AWS::Lambda::Permission", "fn|nope"); !errors.Is(err, ErrAbsent) {
		t.Fatalf("a statement the policy lacks = %v, want absent", err)
	}
	statements = nil
	if _, err := client.ReadByID(ctx, "AWS::Lambda::Permission", "fn|other"); !errors.Is(err, ErrAbsent) {
		t.Fatalf("a function with no policy = %v, want absent", err)
	}
}

// A composite identifier is its parts joined by a bar in the schema's
// order, and a single one is its own value however it is written.
func TestCompositeIdentifiers(t *testing.T) {
	r := readers["AWS::Lambda::Permission"]
	got, err := r.identifierValues("arn:aws:lambda:us-east-1:1:function:fn:live|sid-1")
	if err != nil || !reflect.DeepEqual(got, map[string]string{"FunctionName": "arn:aws:lambda:us-east-1:1:function:fn:live", "Id": "sid-1"}) {
		t.Fatalf("identifierValues = %v, %v", got, err)
	}
	for _, bad := range []string{"fn", "fn|sid|more", ""} {
		if _, err := r.identifierValues(bad); err == nil {
			t.Errorf("identifierValues(%q) succeeded, want an error", bad)
		}
	}
	single := readers["AWS::Lambda::Url"]
	if got, err := single.identifierValues("a|b"); err != nil || got["FunctionArn"] != "a|b" {
		t.Errorf("a single identifier containing a bar = %v, %v; want it kept whole", got, err)
	}
}

// A document extraction follows keys and selections, takes the first path
// that finds a value, reads a lone object as the one element, and tells a
// missing statement, which is absence, from a missing key, which is only no
// value.
func TestDocumentExtraction(t *testing.T) {
	const doc = `{"Statement":[
		{"Sid":"a","Principal":{"AWS":"arn:aws:iam::1:root"},"Condition":{"Bool":{"x":"true"}}},
		{"Sid":"b","Principal":{"Service":"s3.amazonaws.com"}},
		{"Sid":"b","Principal":{"Service":"dup"}}]}`
	lone := `{"Statement":{"Sid":"a","Principal":{"AWS":"x"}}}`
	field := func(paths ...string) Field { return Field{Property: "P", Member: "Policy", Extract: paths} }
	cases := []struct {
		name       string
		doc        string
		field      Field
		want       any
		wantFound  bool
		wantAbsent bool
		wantErr    bool
	}{
		{"the first path with a value", doc, field("Statement[Sid=a].Principal.Service", "Statement[Sid=a].Principal.AWS"), "arn:aws:iam::1:root", true, false, false},
		{"a missing key is no value", doc, field("Statement[Sid=a].Principal.Service"), nil, false, false, false},
		{"a statement the document lacks", doc, field("Statement[Sid=zz].Principal.AWS"), nil, false, true, false},
		{"a statement selected twice", doc, field("Statement[Sid=b].Principal.Service"), nil, false, false, true},
		{"a lone object statement", lone, field("Statement[Sid=a].Principal.AWS"), "x", true, false, false},
		{"a transform of the value", doc, Field{Property: "P", Member: "Policy", Transform: "boolean", Extract: []string{"Statement[Sid=a].Condition.Bool.x"}}, true, true, false, false},
		{"text that is not JSON", "not json", field("Statement[Sid=a].Sid"), nil, false, false, true},
		{"a step through a scalar", doc, field("Statement[Sid=a].Sid.deeper"), nil, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &walk{}
			got, found := Reader{}.extract(w, c.field, c.doc)
			if found != c.wantFound || got != c.want || w.absent != c.wantAbsent || (len(w.errs) > 0) != c.wantErr {
				t.Fatalf("extract = %v, %v, absent %v, errors %v; want %v, %v, %v, error %v", got, found, w.absent, w.errs, c.want, c.wantFound, c.wantAbsent, c.wantErr)
			}
		})
	}
	// The identifier fills the selection.
	w := &walk{vars: map[string]string{"Id": "a"}}
	if got, found := (Reader{}).extract(w, field("Statement[Sid={Id}].Principal.AWS"), lone); !found || got != "x" {
		t.Fatalf("a selection by {Id} = %v, %v", got, found)
	}
}

func TestCompileRefusesMisusedReadVocabulary(t *testing.T) {
	cases := map[string]struct{ file, old, replacement, want string }{
		"a selection by a value that is no identifier": {"AWS--Lambda--Permission.yaml", "Statement[Sid={Id}].Action", "Statement[Sid={Nope}].Action", "extracts by {Nope}, which is not the primary identifier"},
		"a step that is not a key or a selection":      {"AWS--Lambda--Permission.yaml", "Statement[Sid={Id}].Action", "Statement[Sid].Action", "is not key[member=value]"},
		"an empty step":                               {"AWS--Lambda--Permission.yaml", "Statement[Sid={Id}].Action", "Statement[Sid={Id}]..Action", "which has an empty step"},
		"a boolean the schema does not type":          {"AWS--Lambda--Permission.yaml", "    extract: [\"Statement[Sid={Id}].Action\"]", "    extract: [\"Statement[Sid={Id}].Action\"]\n    transform: boolean", "is read as a boolean, but the schema does not type it one"},
		"an extract that also reshapes":               {"AWS--Lambda--Permission.yaml", "    extract: [\"Statement[Sid={Id}].Action\"]", "    extract: [\"Statement[Sid={Id}].Action\"]\n    default: x", "also reshapes it"},
		"an extract of a member that is no string":    {"AWS--Lambda--Url.yaml", "  Cors:\n    member: Cors\n", "  Cors:\n    member: Cors\n    extract: [\"a\"]\n", "extracts from Cors, which is not a string"},
		"an arnPart of a member that is not a string": {"AWS--Lambda--Url.yaml", "    member: FunctionArn\n    transform: arnPart:7", "    member: Cors\n    transform: arnPart:7", "transforms Cors, which is not a string"},
		"an arnPart that is not a number":             {"AWS--Lambda--Url.yaml", "arnPart:7", "arnPart:last", `names transform "arnPart:last"`},
		"a document read under a query protocol":      {"AWS--EC2--VPC.yaml", "  CidrBlock: CidrBlock\n", "  CidrBlock:\n    member: CidrBlock\n    extract: [\"a\"]\n", "extracts from a document, which this client does not read under ec2Query"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(edit(t, c.file, c.old, c.replacement))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant an error containing %q", err, c.want)
			}
		})
	}
}
