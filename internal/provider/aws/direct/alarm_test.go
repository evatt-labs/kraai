package direct

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// An alarm's evaluation window and criteria are unions: whichever member
// the service sets is read as a structure's would be, an empty one whole.
func TestReadAlarmUnions(t *testing.T) {
	const arn = "arn:aws:cloudwatch:us-east-1:1:alarm:a"
	for name, c := range map[string]struct {
		window string
		want   map[string]any
	}{
		"wall clock": {`{"WallClockWindow":{"Timezone":"UTC"}}`, map[string]any{"WallClockWindow": map[string]any{"Timezone": "UTC"}}},
		"sliding":    {`{"SlidingWindow":{}}`, map[string]any{"SlidingWindow": map[string]any{}}},
	} {
		t.Run(name, func(t *testing.T) {
			client, seen := targetServer(t, map[string]string{
				"DescribeAlarms": `{"MetricAlarms":[{"AlarmName":"a","AlarmArn":"` + arn + `","EvaluationWindow":` + c.window + `,` +
					`"EvaluationCriteria":{"PromQLCriteria":{"Query":"up == 0","PendingPeriod":60}}}]}`,
				"ListTagsForResource": `{"Tags":[{"Key":"team","Value":"cloud"}]}`,
			})
			got, err := client.Read(context.Background(), "AWS::CloudWatch::Alarm", map[string]string{"AlarmName": "a"})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"AlarmName": "a", "Arn": arn, "EvaluationWindow": c.want,
				"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": "up == 0", "PendingPeriod": json.Number("60")}},
				"Tags":               []any{map[string]any{"Key": "team", "Value": "cloud"}},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Read = %#v\nwant   %#v", got, want)
			}
			if !strings.Contains(seen["ListTagsForResource"], arn) {
				t.Fatalf("tags request = %s, want the captured ARN", seen["ListTagsForResource"])
			}
		})
	}
}
