package direct

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// s3Echo is a bucket that keeps each configuration put to it and answers
// a read of it with the document put, as S3 does for the configurations
// whose read and write share a shape; every other read is TestReadS3Bucket's.
type s3Echo struct {
	mu     sync.Mutex
	puts   map[string]string
	header map[string]http.Header
	// stored, when set, is what S3 keeps of a document put, such as its
	// filter rule names respelled.
	stored func(string) string
	// deleted is each subresource deleted, which reads as its absence.
	deleted []string
}

func (s *s3Echo) client(t *testing.T) *Client {
	t.Helper()
	s.puts, s.header = map[string]string{}, map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := strings.TrimSuffix(strings.SplitN(r.URL.RawQuery, "&", 2)[0], "=")
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Method == http.MethodPut {
			s.puts[query], s.header[query] = string(body), r.Header.Clone()
			return
		}
		if r.Method == http.MethodDelete {
			delete(s.puts, query)
			s.deleted = append(s.deleted, query)
			return
		}
		if query == "tagging" && slices.Contains(s.deleted, query) {
			status, out := s3NotFound("NoSuchTagSet")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, out)
			return
		}
		if put, ok := s.puts[query]; ok {
			if s.stored != nil {
				put = s.stored(put)
			}
			if size := s.header[query].Get("X-Amz-Transition-Default-Minimum-Object-Size"); size != "" {
				w.Header().Set("X-Amz-Transition-Default-Minimum-Object-Size", size)
			}
			_, _ = io.WriteString(w, put)
			return
		}
		if code, unset := s3Unset[query]; unset {
			status, out := s3NotFound(code)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, out)
			return
		}
		_, _ = io.WriteString(w, s3Bodies[query])
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "eu-west-1", RetryDelay: time.Millisecond, Poll: time.Millisecond, Wait: time.Second,
		Endpoint: func(string) string { return srv.URL },
	}
}

// Each configuration whose read flattens or spreads what S3 sends is put
// in the shape S3 takes, and reads back as the value written: an update's
// wait sees it, and a read returns it unchanged.
func TestUpdateS3BucketRebuildsFlattenedConfigurations(t *testing.T) {
	for name, tc := range map[string]struct {
		query, property, value string
		// sent is what the put's body must carry.
		sent []string
	}{
		"a lifecycle rule on a prefix alone": {"lifecycle", "LifecycleConfiguration",
			`{"Rules": [{"Id": "p", "Status": "Enabled", "Prefix": "logs/", "ExpirationInDays": 30}]}`,
			[]string{`<Rule><Expiration><Days>30</Days></Expiration><Filter><Prefix>logs/</Prefix></Filter><ID>p</ID><Status>Enabled</Status></Rule>`}},
		"a lifecycle rule on one tag": {"lifecycle", "LifecycleConfiguration",
			`{"Rules": [{"Id": "t", "Status": "Enabled", "TagFilters": [{"Key": "a", "Value": "1"}], "ExpirationInDays": 7}]}`,
			[]string{`<Filter><Tag><Key>a</Key><Value>1</Value></Tag></Filter>`}},
		"a lifecycle rule on a size alone": {"lifecycle", "LifecycleConfiguration",
			`{"Rules": [{"Id": "s", "Status": "Enabled", "ObjectSizeGreaterThan": "1024", "ExpirationInDays": 7}]}`,
			[]string{`<Filter><ObjectSizeGreaterThan>1024</ObjectSizeGreaterThan></Filter>`}},
		"a lifecycle rule on two tags": {"lifecycle", "LifecycleConfiguration",
			`{"Rules": [{"Id": "tt", "Status": "Enabled", "TagFilters": [{"Key": "a", "Value": "1"}, {"Key": "b", "Value": "2"}], "ExpirationInDays": 7}]}`,
			[]string{`<Filter><And><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag></And></Filter>`}},
		"a lifecycle rule on a prefix, a tag and a size": {"lifecycle", "LifecycleConfiguration",
			`{"Rules": [{"Id": "and", "Status": "Enabled", "Prefix": "tmp/", "TagFilters": [{"Key": "d", "Value": "4"}], "ObjectSizeGreaterThan": "1024", "ObjectSizeLessThan": "4096", "ExpirationInDays": 30}]}`,
			[]string{`<Filter><And><ObjectSizeGreaterThan>1024</ObjectSizeGreaterThan><ObjectSizeLessThan>4096</ObjectSizeLessThan><Prefix>tmp/</Prefix><Tag><Key>d</Key><Value>4</Value></Tag></And></Filter>`}},
		"a lifecycle rule on nothing, with every action and the minimum size": {"lifecycle", "LifecycleConfiguration",
			`{"TransitionDefaultMinimumObjectSize": "all_storage_classes_128K", "Rules": [{"Id": "all", "Status": "Disabled",
				"Transitions": [{"StorageClass": "STANDARD_IA", "TransitionInDays": 40}],
				"NoncurrentVersionExpiration": {"NoncurrentDays": 7, "NewerNoncurrentVersions": 2},
				"NoncurrentVersionTransitions": [{"StorageClass": "GLACIER", "TransitionInDays": 60}],
				"AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 3}}]}`,
			[]string{`<Filter></Filter>`, `<Transition><Days>40</Days><StorageClass>STANDARD_IA</StorageClass></Transition>`, `<NoncurrentVersionTransition><NoncurrentDays>60</NoncurrentDays><StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition>`}},
		"notifications, one configuration per event": {"notification", "NotificationConfiguration",
			`{"EventBridgeConfiguration": {"EventBridgeEnabled": true},
			  "TopicConfigurations": [
				{"Event": "s3:ObjectCreated:*", "Topic": "arn:aws:sns:eu-west-1:111122223333:t", "Filter": {"S3Key": {"Rules": [{"Name": "Prefix", "Value": "in/"}]}}},
				{"Event": "s3:ObjectRemoved:*", "Topic": "arn:aws:sns:eu-west-1:111122223333:t"}],
			  "QueueConfigurations": [{"Event": "s3:ObjectRestore:Completed", "Queue": "arn:aws:sqs:eu-west-1:111122223333:q"}],
			  "LambdaConfigurations": [{"Event": "s3:ObjectCreated:Put", "Function": "arn:aws:lambda:eu-west-1:111122223333:function:f"}]}`,
			[]string{`<EventBridgeConfiguration></EventBridgeConfiguration>`,
				`<TopicConfiguration><Event>s3:ObjectCreated:*</Event><Filter><S3Key><FilterRule><Name>Prefix</Name><Value>in/</Value></FilterRule></S3Key></Filter><Topic>arn:aws:sns:eu-west-1:111122223333:t</Topic></TopicConfiguration><TopicConfiguration><Event>s3:ObjectRemoved:*</Event><Topic>arn:aws:sns:eu-west-1:111122223333:t</Topic></TopicConfiguration>`,
				`<CloudFunctionConfiguration><Event>s3:ObjectCreated:Put</Event><CloudFunction>arn:aws:lambda:eu-west-1:111122223333:function:f</CloudFunction></CloudFunctionConfiguration>`}},
		"notifications with EventBridge off": {"notification", "NotificationConfiguration",
			`{"EventBridgeConfiguration": {"EventBridgeEnabled": false}, "QueueConfigurations": [{"Event": "s3:ObjectRestore:Completed", "Queue": "arn:aws:sqs:eu-west-1:111122223333:q"}]}`,
			[]string{`<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><QueueConfiguration>`}},
		"replication": {"replication", "ReplicationConfiguration",
			`{"Role": "arn:aws:iam::111122223333:role/r", "Rules": [{"Id": "r1", "Priority": 1, "Status": "Enabled",
				"Filter": {"And": {"Prefix": "a/", "TagFilters": [{"Key": "k", "Value": "v"}]}},
				"DeleteMarkerReplication": {"Status": "Disabled"},
				"Destination": {"Bucket": "arn:aws:s3:::dest", "StorageClass": "STANDARD_IA"}}]}`,
			[]string{`<Role>arn:aws:iam::111122223333:role/r</Role>`, `<Filter><And><Prefix>a/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter>`}},
	} {
		t.Run(name, func(t *testing.T) {
			s := &s3Echo{}
			client := s.client(t)
			var value any
			if err := json.Unmarshal([]byte(tc.value), &value); err != nil {
				t.Fatal(err)
			}
			if err := client.Update(context.Background(), "AWS::S3::Bucket", "b", map[string]any{}, map[string]any{tc.property: value}); err != nil {
				t.Fatal(err)
			}
			body := s.puts[tc.query]
			for _, want := range tc.sent {
				if !strings.Contains(body, want) {
					t.Errorf("put body\n%s\nlacks\n%s", body, want)
				}
			}
			got, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "b"})
			if err != nil {
				t.Fatal(err)
			}
			want := value
			if name == "notifications with EventBridge off" {
				// Off is written as no EventBridge configuration, which the
				// read gives as the empty structure holding none.
				want = map[string]any{"EventBridgeConfiguration": map[string]any{}, "QueueConfigurations": value.(map[string]any)["QueueConfigurations"]}
			}
			if !reflect.DeepEqual(jsonValue(got[tc.property]), jsonValue(want)) {
				t.Errorf("read back %s\nwant      %s", mustJSON(t, got[tc.property]), mustJSON(t, want))
			}
		})
	}
}

// A notification filter rule named in lower case is seen by the wait once
// S3 has written it back as Prefix: the update succeeds rather than waiting
// out its time for a spelling S3 never returns.
func TestUpdateS3NotificationRuleNameInAnyCase(t *testing.T) {
	s := &s3Echo{stored: func(doc string) string {
		return strings.NewReplacer("<Name>prefix</Name>", "<Name>Prefix</Name>", "<Name>SUFFIX</Name>", "<Name>Suffix</Name>").Replace(doc)
	}}
	client := s.client(t)
	value := map[string]any{"QueueConfigurations": []any{map[string]any{
		"Event": "s3:ObjectCreated:*", "Queue": "arn:aws:sqs:eu-west-1:111122223333:q",
		"Filter": map[string]any{"S3Key": map[string]any{"Rules": []any{
			map[string]any{"Name": "prefix", "Value": "in/"}, map[string]any{"Name": "SUFFIX", "Value": ".csv"}}}}}}}
	if err := client.Update(context.Background(), "AWS::S3::Bucket", "b", map[string]any{}, map[string]any{"NotificationConfiguration": value}); err != nil {
		t.Fatal(err)
	}
	if body := s.puts["notification"]; !strings.Contains(body, "<Name>prefix</Name>") {
		t.Errorf("put body\n%s\nwant the name as the manifest spells it", body)
	}
}

// No tags is the bucket's tagging deleted, since S3 refuses an empty tag
// set; a read then finds none.
func TestUpdateS3BucketToNoTags(t *testing.T) {
	s := &s3Echo{}
	client := s.client(t)
	tags := []any{map[string]any{"Key": "team", "Value": "core"}}
	if err := client.Update(context.Background(), "AWS::S3::Bucket", "b", map[string]any{}, map[string]any{"Tags": tags}); err != nil {
		t.Fatal(err)
	}
	if err := client.Update(context.Background(), "AWS::S3::Bucket", "b", map[string]any{"Tags": tags}, map[string]any{"Tags": []any{}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.deleted, []string{"tagging"}) {
		t.Errorf("deleted = %v, want the tagging alone", s.deleted)
	}
	got, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if v, read := got["Tags"]; read {
		t.Errorf("Tags read back as %v, want none", v)
	}
}
