package direct

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestCompileRefusesACall(t *testing.T) {
	const file = "AWS--ElasticLoadBalancingV2--TargetGroup.yaml"
	cases := map[string]struct {
		old, replacement, want string
	}{
		"a property mapped by two calls": {"      TargetGroupAttributes:\n        member: Attributes",
			"      Tags:\n        member: Attributes", "Tags is mapped by more than one call"},
		"a property mapped by the read and a call": {"  TargetGroupName: TargetGroupName\n",
			"  TargetGroupName: TargetGroupName\n  Tags: TargetGroupName\n", "Tags is mapped by more than one call"},
		"a call that maps nothing": {"    properties:\n      TargetGroupAttributes:\n        member: Attributes\n        properties:\n          Key: Key\n          Value: Value\n",
			"    properties: {}\n", "DescribeTargetGroupAttributes maps no property"},
		"an unknown transform": {"transform: arnResource", "transform: upper", `names transform "upper"`},
		"a transform of a non-string": {"    member: TargetGroupArn\n    transform: arnResource",
			"    member: Port\n    transform: arnResource", "transforms Port, which is not a string"},
		"a list step through a structure": {"member: TargetHealthDescriptions[].Target",
			"member: TargetHealthDescriptions[].Target[].Id", "but Target is not a list"},
		"a list walked without []": {"member: TargetHealthDescriptions[].Target",
			"member: TargetHealthDescriptions.Target", "but TargetHealthDescriptions is a list; mark it TargetHealthDescriptions[]"},
		"a projection onto a property that is not an array": {"      Targets:\n        member: TargetHealthDescriptions[].Target",
			"      TargetGroupName:\n        member: TargetHealthDescriptions[].Target", "maps through a list, but the schema does not declare it an array"},
		"a call's operation the model lacks": {"operation: DescribeTargetHealth", "operation: DescribeTargetHealthy", "also[2] DescribeTargetHealthy"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(edit(t, file, c.old, c.replacement))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant an error containing %q", err, c.want)
			}
		})
	}
}

func TestTransformArnResource(t *testing.T) {
	for in, want := range map[any]any{
		"arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/tg/abc": "targetgroup/tg/abc",
		"arn:aws:s3:::bucket/key:with:colons":                         "bucket/key:with:colons",
		"not-an-arn":                                                  "not-an-arn",
		42:                                                            42,
	} {
		if got := transform("arnResource", in); got != want {
			t.Errorf("transform(%v) = %v, want %v", in, got, want)
		}
	}
}

// A further call that finds nothing is an error, never absence: the main
// read found the instance, so production must fall back rather than
// report it gone.
func TestAFurtherCallFindingNothingIsNotAbsence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		if form.Get("Action") == "DescribeTags" {
			_, _ = io.WriteString(w, `<DescribeTagsResponse><DescribeTagsResult><TagDescriptions/></DescribeTagsResult></DescribeTagsResponse>`)
			return
		}
		_, _ = io.WriteString(w, targetGroupXML)
	}))
	t.Cleanup(srv.Close)
	client := &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL },
	}
	_, err := client.Read(context.Background(), targetGroups, map[string]string{"TargetGroupArn": "arn:tg"})
	if err == nil || errors.Is(err, ErrAbsent) {
		t.Fatalf("Read = %v, want an error that is not ErrAbsent", err)
	}
}
