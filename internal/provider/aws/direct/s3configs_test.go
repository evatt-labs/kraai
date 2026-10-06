package direct

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// s3ListRoots is the root a list of each subresource's configurations is
// answered in.
var s3ListRoots = map[string]string{
	"analytics":           "ListBucketAnalyticsConfigurationResult",
	"intelligent-tiering": "ListBucketIntelligentTieringConfigurationsOutput",
	"inventory":           "ListInventoryConfigurationsResult",
	"metrics":             "ListMetricsConfigurationsResult",
}

// s3Configs is a bucket keeping the configurations put to it by id, as S3
// keeps analytics, intelligent-tiering, inventory and metrics ones, and
// answering a list of them with each document put; every other read is
// TestReadS3Bucket's.
type s3Configs struct {
	mu    sync.Mutex
	held  map[string]map[string]string
	calls []string
}

func (s *s3Configs) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q, _ := url.ParseQuery(r.URL.RawQuery)
		// The subresource is the one query key that is not a parameter.
		var sub string
		for k := range q {
			if k != "id" && k != "x-id" && k != "continuation-token" {
				sub = k
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		held, kept := s.held[sub]
		switch {
		case r.Method != http.MethodGet && kept:
			s.calls = append(s.calls, r.Method+" "+sub+" "+q.Get("id"))
			if r.Method == http.MethodPut {
				held[q.Get("id")] = string(body)
			} else {
				delete(held, q.Get("id"))
			}
			return
		case r.Method != http.MethodGet:
			t.Errorf("%s %s, a subresource the fake keeps nothing of", r.Method, sub)
			return
		case kept:
			root := s3ListRoots[sub]
			out := "<" + root + s3NS + "><IsTruncated>false</IsTruncated>"
			for _, id := range sortedKeys(held) {
				out += strings.Replace(held[id], s3NS, "", 1)
			}
			_, _ = io.WriteString(w, out+"</"+root+">")
			return
		}
		if code, unset := s3Unset[sub]; unset {
			status, out := s3NotFound(code)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, out)
			return
		}
		_, _ = io.WriteString(w, s3Bodies[sub])
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "eu-west-1", RetryDelay: time.Millisecond, Poll: time.Millisecond, Wait: time.Second,
		Endpoint: func(string) string { return srv.URL },
	}
}

// Each configuration kept by id is put when new or changed and deleted
// when no longer wanted, one call each, and the list reads back as the
// configurations wanted, their filters rebuilt from the flattened schema.
func TestUpdateS3ConfigurationsById(t *testing.T) {
	for name, tc := range map[string]struct {
		sub, property    string
		current, desired string
		calls            []string
		sent             []string
	}{
		"metrics": {"metrics", "MetricsConfigurations",
			`[{"Id": "keep", "Prefix": "k/"}, {"Id": "change", "Prefix": "c/"}, {"Id": "gone", "Prefix": "g/"}]`,
			`[{"Id": "keep", "Prefix": "k/"}, {"Id": "change", "Prefix": "c/", "TagFilters": [{"Key": "a", "Value": "1"}]}, {"Id": "new", "AccessPointArn": "arn:aws:s3:eu-west-1:111122223333:accesspoint/ap"}]`,
			[]string{"DELETE metrics gone", "PUT metrics change", "PUT metrics new"},
			[]string{`<Filter><And><Prefix>c/</Prefix><Tag><Key>a</Key><Value>1</Value></Tag></And></Filter>`, `<Filter><AccessPointArn>arn:aws:s3:eu-west-1:111122223333:accesspoint/ap</AccessPointArn></Filter>`}},
		"analytics": {"analytics", "AnalyticsConfigurations", `[]`,
			`[{"Id": "an1", "TagFilters": [{"Key": "a", "Value": "1"}], "StorageClassAnalysis": {"DataExport": {"OutputSchemaVersion": "V_1", "Destination": {"BucketArn": "arn:aws:s3:::dest", "Format": "CSV", "Prefix": "an/"}}}}]`,
			[]string{"PUT analytics an1"},
			[]string{`<Filter><Tag><Key>a</Key><Value>1</Value></Tag></Filter>`, `<Destination><S3BucketDestination><Bucket>arn:aws:s3:::dest</Bucket><Format>CSV</Format><Prefix>an/</Prefix></S3BucketDestination></Destination>`}},
		"intelligent tiering": {"intelligent-tiering", "IntelligentTieringConfigurations", `[]`,
			`[{"Id": "it1", "Status": "Enabled", "Tierings": [{"AccessTier": "ARCHIVE_ACCESS", "Days": 90}]}]`,
			[]string{"PUT intelligent-tiering it1"},
			[]string{`<Tiering><AccessTier>ARCHIVE_ACCESS</AccessTier><Days>90</Days></Tiering>`}},
		"inventory": {"inventory", "InventoryConfigurations", `[]`,
			`[{"Id": "inv1", "Enabled": true, "IncludedObjectVersions": "Current", "ScheduleFrequency": "Weekly", "Prefix": "data/", "OptionalFields": ["Size"],
			   "Destination": {"BucketArn": "arn:aws:s3:::dest", "Format": "CSV", "Prefix": "inventory", "BucketAccountId": "111122223333"}}]`,
			[]string{"PUT inventory inv1"},
			[]string{`<Filter><Prefix>data/</Prefix></Filter>`, `<Schedule><Frequency>Weekly</Frequency></Schedule>`, `<IsEnabled>true</IsEnabled>`}},
	} {
		t.Run(name, func(t *testing.T) {
			s := &s3Configs{held: map[string]map[string]string{tc.sub: {}}}
			client := s.client(t)
			var current, desired []any
			if err := json.Unmarshal([]byte(tc.current), &current); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.desired), &desired); err != nil {
				t.Fatal(err)
			}
			// The bucket starts holding current, put through the same route.
			if len(current) > 0 {
				if err := client.Update(context.Background(), "AWS::S3::Bucket", "b", map[string]any{}, map[string]any{tc.property: current}); err != nil {
					t.Fatal(err)
				}
				s.calls = nil
			}
			before := map[string]string{}
			for id, doc := range s.held[tc.sub] {
				before[id] = doc
			}
			if err := client.Update(context.Background(), "AWS::S3::Bucket", "b", map[string]any{tc.property: current}, map[string]any{tc.property: desired}); err != nil {
				t.Fatal(err)
			}
			calls := slices.Clone(s.calls)
			slices.Sort(calls)
			if !slices.Equal(calls, tc.calls) {
				t.Errorf("calls = %v, want %v", calls, tc.calls)
			}
			var sent strings.Builder
			for _, id := range sortedKeys(s.held[tc.sub]) {
				sent.WriteString(s.held[tc.sub][id])
			}
			for _, want := range tc.sent {
				if !strings.Contains(sent.String(), want) {
					t.Errorf("configurations held\n%s\nlack\n%s", sent.String(), want)
				}
			}
			got, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "b"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(jsonValue(sortedByID(got[tc.property])), jsonValue(sortedByID(desired))) {
				t.Errorf("read back %s\nwant      %s", mustJSON(t, got[tc.property]), mustJSON(t, desired))
			}
		})
	}
}

// sortedByID is a list of configurations ordered by Id, the order S3
// lists them in.
func sortedByID(v any) []any {
	items := slices.Clone(asList(v))
	slices.SortFunc(items, func(a, b any) int {
		ia, _ := a.(map[string]any)["Id"].(string)
		ib, _ := b.(map[string]any)["Id"].(string)
		return strings.Compare(ia, ib)
	})
	return items
}
