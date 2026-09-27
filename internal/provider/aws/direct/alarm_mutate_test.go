package direct

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const alarmType = "AWS::CloudWatch::Alarm"

// fakeAlarms is one metric alarm, which PutMetricAlarm replaces whole, as
// CloudWatch does: a member it is not sent is unset.
type fakeAlarms struct {
	mu    sync.Mutex
	alarm map[string]any
	tags  []any
	calls map[string][]map[string]any
}

func (f *fakeAlarms) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		op := r.Header.Get("X-Amz-Target")
		op = op[strings.LastIndex(op, ".")+1:]
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[op] = append(f.calls[op], in)
		switch op {
		case "PutMetricAlarm":
			f.alarm = map[string]any{"AlarmArn": "arn:aws:cloudwatch:us-east-1:1:alarm:" + in["AlarmName"].(string)}
			if tags, ok := in["Tags"].([]any); ok {
				f.tags = tags
			}
			for k, v := range in {
				if k != "Tags" {
					f.alarm[k] = v
				}
			}
			// CloudWatch answers a threshold as a decimal.
			if th, ok := in["Threshold"].(float64); ok {
				f.alarm["Threshold"] = json.Number(fmt.Sprintf("%.1f", th))
			}
		case "DescribeAlarms":
			alarms := []any{}
			if f.alarm != nil {
				alarms = append(alarms, f.alarm)
			}
			body, _ := json.Marshal(map[string]any{"MetricAlarms": alarms})
			_, _ = w.Write(body)
			return
		case "ListTagsForResource":
			body, _ := json.Marshal(map[string]any{"Tags": append([]any{}, f.tags...)})
			_, _ = w.Write(body)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// PutMetricAlarm replaces the whole alarm, so an update of one property
// sends every other one as it was read, and one never read is left unset
// rather than refused: only the name is required.
func TestUpdateAlarmSendsTheWholeAlarm(t *testing.T) {
	f := &fakeAlarms{alarm: map[string]any{"AlarmName": "kraai-e-alarm"}}
	client := f.serve(t)
	current := map[string]any{
		"AlarmName": "kraai-e-alarm", "Namespace": "kraai/e", "MetricName": "Probe", "Statistic": "Average",
		"Period": json.Number("60"), "EvaluationPeriods": json.Number("1"), "Threshold": json.Number("1.0"),
		"ComparisonOperator": "GreaterThanThreshold", "Dimensions": []any{map[string]any{"Name": "env", "Value": "e"}},
	}
	if err := client.Update(context.Background(), alarmType, "kraai-e-alarm", current, map[string]any{"Period": 300}); err != nil {
		t.Fatal(err)
	}
	sent := f.calls["PutMetricAlarm"][0]
	want := map[string]any{
		"AlarmName": "kraai-e-alarm", "Namespace": "kraai/e", "MetricName": "Probe", "Statistic": "Average",
		"Period": float64(300), "EvaluationPeriods": float64(1), "Threshold": float64(1),
		"ComparisonOperator": "GreaterThanThreshold", "Dimensions": []any{map[string]any{"Name": "env", "Value": "e"}},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("PutMetricAlarm input:\n got %v\nwant %v", sent, want)
	}
}

// A metric math alarm reads its dimensions back as an empty list, which
// PutMetricAlarm refuses beside Metrics; an optional property read empty is
// unset, so it is left out.
func TestUpdateAlarmLeavesOutAnEmptyOptionalProperty(t *testing.T) {
	f := &fakeAlarms{alarm: map[string]any{"AlarmName": "kraai-e-alarm"}}
	client := f.serve(t)
	metrics := []any{map[string]any{"Id": "m1", "ReturnData": true}}
	current := map[string]any{"AlarmName": "kraai-e-alarm", "ComparisonOperator": "GreaterThanThreshold",
		"EvaluationPeriods": json.Number("1"), "Threshold": json.Number("1.0"), "Metrics": metrics,
		"Dimensions": []any{}, "OKActions": []any{}}
	if err := client.Update(context.Background(), alarmType, "kraai-e-alarm", current, map[string]any{"Threshold": 2}); err != nil {
		t.Fatal(err)
	}
	sent := f.calls["PutMetricAlarm"][0]
	for _, member := range []string{"Dimensions", "OKActions"} {
		if v, ok := sent[member]; ok {
			t.Errorf("%s sent as %v, want it left out", member, v)
		}
	}
	if _, ok := sent["Metrics"]; !ok {
		t.Error("Metrics left out, want it sent as read")
	}
}

// The wait after a create compares numbers by value: CloudWatch reads a
// threshold of 1 back as 1.0.
func TestCreateAlarmSeesADecimalThreshold(t *testing.T) {
	f := &fakeAlarms{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), alarmType, map[string]any{
		"Namespace": "kraai/e", "MetricName": "Probe", "Statistic": "Average", "Period": 60,
		"EvaluationPeriods": 1, "Threshold": 1, "ComparisonOperator": "GreaterThanThreshold",
		"Tags": []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-alarm"}},
	})
	if err != nil || id != "kraai-e-alarm" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if th := f.alarm["Threshold"]; th != json.Number("1.0") {
		t.Fatalf("fake threshold = %#v, want the decimal the service answers", th)
	}
}

func TestCoversComparesNumbersByValue(t *testing.T) {
	for _, c := range []struct {
		desired, current any
		want             bool
	}{
		{1, json.Number("1.0"), true},
		{0.5, json.Number("0.50"), true},
		{1, json.Number("1.5"), false},
		{map[string]any{"n": 2}, map[string]any{"n": json.Number("2.00")}, true},
	} {
		if got := covers(c.desired, c.current); got != c.want {
			t.Errorf("covers(%#v, %#v) = %v, want %v", c.desired, c.current, got, c.want)
		}
	}
}
