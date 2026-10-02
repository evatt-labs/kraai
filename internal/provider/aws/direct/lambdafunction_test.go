package direct

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestOnlyWriteOnlyMembers(t *testing.T) {
	schema := cfnSchema{
		Properties: map[string]cfnProperty{
			"Code":  {Properties: map[string]cfnProperty{"S3Key": {}, "ZipFile": {}}},
			"Mixed": {Properties: map[string]cfnProperty{"A": {}, "B": {}}},
			"Plain": {},
		},
		WriteOnlyPointers: []string{"/properties/Code/S3Key", "/properties/Code/ZipFile", "/properties/Mixed/A"},
	}
	for name, want := range map[string]bool{"Code": true, "Mixed": false, "Plain": false, "Absent": false} {
		if got := schema.onlyWriteOnlyMembers(name); got != want {
			t.Errorf("onlyWriteOnlyMembers(%s) = %v, want %v", name, got, want)
		}
	}
}

// A function's Code is all write-only in the schema, while GetFunction
// answers with a presigned download URL for it: the read must not carry it.
func TestLambdaFunctionReadOmitsCodeLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/code-signing-config"):
			_, _ = w.Write([]byte(`{"CodeSigningConfigArn":null,"FunctionName":null}`))
		case strings.HasSuffix(r.URL.Path, "/recursion-config"):
			_, _ = w.Write([]byte(`{"RecursiveLoop":"Terminate"}`))
		case strings.HasSuffix(r.URL.Path, "/runtime-management-config"):
			_, _ = w.Write([]byte(`{"UpdateRuntimeOn":"Auto"}`))
		case strings.HasSuffix(r.URL.Path, "/function-scaling-config"):
			w.Header().Set("X-Amzn-Errortype", "ResourceNotFoundException")
			w.WriteHeader(404)
		default:
			_, _ = w.Write([]byte(`{"Configuration":{"FunctionName":"f","FunctionArn":"arn:f","Timeout":3},` +
				`"Code":{"RepositoryType":"S3","Location":"https://example.invalid/?X-Amz-Security-Token=secret"},` +
				`"Tags":{"a":"b"},"Concurrency":{"ReservedConcurrentExecutions":2}}`))
		}
	}))
	t.Cleanup(srv.Close)
	client := &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKID", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }}
	got, err := client.Read(context.Background(), "AWS::Lambda::Function", map[string]string{"FunctionName": "f"})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := got["Code"]; leaked {
		t.Errorf("the read carries Code: %v", got["Code"])
	}
	if got["ReservedConcurrentExecutions"] == nil || got["RecursiveLoop"] != "Terminate" || got["Timeout"] == nil {
		t.Errorf("read = %v", got)
	}
	if _, set := got["CodeSigningConfigArn"]; set {
		t.Errorf("a null signing config reads as set: %v", got["CodeSigningConfigArn"])
	}
}
