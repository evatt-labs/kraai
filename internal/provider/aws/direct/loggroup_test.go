package direct

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

const logGroupType = "AWS::Logs::LogGroup"

// DescribeLogGroups answers a matched log group's own properties, filtered
// by an exact identifier rather than a prefix. A log group with no tags or
// policies reads without them rather than failing.
func TestReadLogGroup(t *testing.T) {
	client, seen := targetServer(t, map[string]string{
		"DescribeLogGroups": `{"logGroups":[{` +
			`"logGroupName":"/aws/lambda/kraai",` +
			`"arn":"arn:aws:logs:us-east-1:409032463870:log-group:/aws/lambda/kraai:*",` +
			`"logGroupArn":"arn:aws:logs:us-east-1:409032463870:log-group:/aws/lambda/kraai",` +
			`"kmsKeyId":"arn:aws:kms:us-east-1:409032463870:key/abc",` +
			`"logGroupClass":"STANDARD",` +
			`"retentionInDays":14,` +
			`"deletionProtectionEnabled":true,` +
			`"bearerTokenAuthenticationEnabled":false` +
			`}]}`,
		"ListTagsForResource":      `{}`,
		"GetDataProtectionPolicy":  `{}`,
		"DescribeIndexPolicies":    `{"indexPolicies":[]}`,
		"DescribeResourcePolicies": `{"resourcePolicies":[]}`,
	})
	got, err := client.Read(context.Background(), logGroupType, map[string]string{"LogGroupName": "/aws/lambda/kraai"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"LogGroupName":                     "/aws/lambda/kraai",
		"Arn":                              "arn:aws:logs:us-east-1:409032463870:log-group:/aws/lambda/kraai:*",
		"KmsKeyId":                         "arn:aws:kms:us-east-1:409032463870:key/abc",
		"LogGroupClass":                    "STANDARD",
		"RetentionInDays":                  json.Number("14"),
		"DeletionProtectionEnabled":        true,
		"BearerTokenAuthenticationEnabled": false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v, want %#v", got, want)
	}
	if seen["DescribeLogGroups"] != `{"logGroupIdentifiers":["/aws/lambda/kraai"]}` {
		t.Fatalf("request = %s, want it to filter by the exact identifier", seen["DescribeLogGroups"])
	}
}

// A name that matches no log group answers an empty list, which is
// absence, not an error: DescribeLogGroups never reports one gone
// instance the way a singular Get would.
func TestReadLogGroupAbsent(t *testing.T) {
	client, _ := bodyPages(t, func(string) (int, string) { return 200, `{"logGroups":[]}` })
	_, err := client.Read(context.Background(), logGroupType, map[string]string{"LogGroupName": "does-not-exist-kraai-probe-xyz"})
	if !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read = %v, want ErrAbsent", err)
	}
}
