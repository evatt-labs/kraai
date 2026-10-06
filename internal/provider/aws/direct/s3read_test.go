package direct

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const s3NS = ` xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`

// s3Bodies answers each bucket operation by the query literal its URI
// carries, with what S3 sends for a bucket that sets every mapped feature.
var s3Bodies = map[string]string{
	"metadataConfiguration": `<GetBucketMetadataConfigurationResult` + s3NS + `><MetadataConfigurationResult>` +
		`<DestinationResult><TableBucketType>aws</TableBucketType><TableBucketArn>arn:aws:s3tables:eu-west-1:111122223333:bucket/aws-s3</TableBucketArn><TableNamespace>b_ns</TableNamespace></DestinationResult>` +
		`<JournalTableConfigurationResult><TableStatus>ACTIVE</TableStatus><TableName>journal</TableName><TableArn>arn:aws:s3tables:eu-west-1:111122223333:bucket/aws-s3/table/j</TableArn><RecordExpiration><Expiration>ENABLED</Expiration><Days>7</Days></RecordExpiration></JournalTableConfigurationResult>` +
		`<InventoryTableConfigurationResult><ConfigurationState>DISABLED</ConfigurationState></InventoryTableConfigurationResult>` +
		`</MetadataConfigurationResult></GetBucketMetadataConfigurationResult>`,
	"notification": `<NotificationConfiguration` + s3NS + `>` +
		`<TopicConfiguration><Id>t1</Id><Topic>arn:aws:sns:eu-west-1:111122223333:t</Topic><Event>s3:ObjectCreated:*</Event><Event>s3:ObjectRemoved:*</Event>` +
		`<Filter><S3Key><FilterRule><Name>Prefix</Name><Value>in/</Value></FilterRule><FilterRule><Name>Suffix</Name><Value>.csv</Value></FilterRule></S3Key></Filter></TopicConfiguration>` +
		`<QueueConfiguration><Id>q1</Id><Queue>arn:aws:sqs:eu-west-1:111122223333:q</Queue><Event>s3:ObjectRestore:Completed</Event></QueueConfiguration>` +
		`<EventBridgeConfiguration></EventBridgeConfiguration></NotificationConfiguration>`,
	"lifecycle": `<LifecycleConfiguration` + s3NS + `>` +
		`<Rule><ID>r1</ID><Filter><And><Prefix>tmp/</Prefix><Tag><Key>d</Key><Value>4</Value></Tag><ObjectSizeGreaterThan>1024</ObjectSizeGreaterThan></And></Filter><Status>Enabled</Status><Expiration><Days>30</Days></Expiration></Rule>` +
		`<Rule><ID>r2</ID><Filter><Prefix>old/</Prefix></Filter><Status>Enabled</Status><Transition><Days>40</Days><StorageClass>STANDARD_IA</StorageClass></Transition><NoncurrentVersionExpiration><NoncurrentDays>7</NoncurrentDays></NoncurrentVersionExpiration></Rule>` +
		`</LifecycleConfiguration>`,
	"versioning":        `<VersioningConfiguration` + s3NS + `><Status>Enabled</Status></VersioningConfiguration>`,
	"accelerate":        `<AccelerateConfiguration` + s3NS + `><Status>Suspended</Status></AccelerateConfiguration>`,
	"abac":              `<AbacStatus` + s3NS + `><Status>Enabled</Status></AbacStatus>`,
	"encryption":        `<ServerSideEncryptionConfiguration` + s3NS + `><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>key-1</KMSMasterKeyID></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>true</BucketKeyEnabled><BlockedEncryptionTypes><EncryptionType>SSE-C</EncryptionType></BlockedEncryptionTypes></Rule></ServerSideEncryptionConfiguration>`,
	"ownershipControls": `<OwnershipControls` + s3NS + `><Rule><ObjectOwnership>BucketOwnerPreferred</ObjectOwnership></Rule></OwnershipControls>`,
	"publicAccessBlock": `<PublicAccessBlockConfiguration` + s3NS + `><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>false</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>false</RestrictPublicBuckets></PublicAccessBlockConfiguration>`,
	"tagging":           `<Tagging` + s3NS + `><TagSet><Tag><Key>team</Key><Value>core</Value></Tag><Tag><Key>env</Key><Value>dev</Value></Tag></TagSet></Tagging>`,
	"metrics": `<ListMetricsConfigurationsResult` + s3NS + `><IsTruncated>false</IsTruncated>` +
		`<MetricsConfiguration><Id>m1</Id><Filter><And><Prefix>data/</Prefix><Tag><Key>c</Key><Value>3</Value></Tag></And></Filter></MetricsConfiguration>` +
		`<MetricsConfiguration><Id>m2</Id><Filter><Tag><Key>a</Key><Value>1</Value></Tag></Filter></MetricsConfiguration>` +
		`<MetricsConfiguration><Id>m3</Id><Filter><AccessPointArn>arn:aws:s3:eu-west-1:111122223333:accesspoint/ap</AccessPointArn></Filter></MetricsConfiguration>` +
		`</ListMetricsConfigurationsResult>`,
	"analytics": `<ListBucketAnalyticsConfigurationResult` + s3NS + `><IsTruncated>false</IsTruncated>` +
		`<AnalyticsConfiguration><Id>an1</Id><Filter><And><Prefix>logs/</Prefix><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag></And></Filter><StorageClassAnalysis/></AnalyticsConfiguration>` +
		`</ListBucketAnalyticsConfigurationResult>`,
	"intelligent-tiering": `<ListBucketIntelligentTieringConfigurationsOutput` + s3NS + `><IsTruncated>false</IsTruncated>` +
		`<IntelligentTieringConfiguration><Id>it1</Id><Filter><Prefix>cold/</Prefix></Filter><Status>Enabled</Status><Tiering><Days>90</Days><AccessTier>ARCHIVE_ACCESS</AccessTier></Tiering></IntelligentTieringConfiguration>` +
		`</ListBucketIntelligentTieringConfigurationsOutput>`,
	"inventory": `<ListInventoryConfigurationsResult` + s3NS + `><IsTruncated>false</IsTruncated></ListInventoryConfigurationsResult>`,
	"logging":   `<BucketLoggingStatus` + s3NS + `><LoggingEnabled><TargetBucket>logs</TargetBucket><TargetPrefix>b/</TargetPrefix><TargetObjectKeyFormat><PartitionedPrefix><PartitionDateSource>EventTime</PartitionDateSource></PartitionedPrefix></TargetObjectKeyFormat></LoggingEnabled></BucketLoggingStatus>`,
	"cors":      `<CORSConfiguration` + s3NS + `><CORSRule><ID>web</ID><AllowedHeader>*</AllowedHeader><AllowedHeader>x-amz-meta-a</AllowedHeader><AllowedMethod>GET</AllowedMethod><AllowedOrigin>https://example.com</AllowedOrigin><ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>3000</MaxAgeSeconds></CORSRule></CORSConfiguration>`,
}

type s3Request struct{ method, path, query, sha, auth string }

func s3Client(t *testing.T, answer func(query string) (int, string)) (*Client, func() []s3Request) {
	t.Helper()
	var mu sync.Mutex
	var seen []s3Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, s3Request{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("X-Amz-Content-Sha256"), r.Header.Get("Authorization")})
		mu.Unlock()
		query := strings.TrimSuffix(strings.SplitN(r.URL.RawQuery, "&", 2)[0], "=")
		status, body := answer(query)
		if status == http.StatusOK {
			for k, v := range s3Headers[query] {
				w.Header().Set(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	client := &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "eu-west-1", RetryDelay: time.Millisecond,
		Endpoint: func(host string) string {
			if host != "s3.eu-west-1.amazonaws.com" {
				t.Errorf("request host %q, want the regional host", host)
			}
			return srv.URL
		},
		Now: func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) },
	}
	return client, func() []s3Request {
		mu.Lock()
		defer mu.Unlock()
		sort.Slice(seen, func(i, j int) bool { return seen[i].query < seen[j].query })
		return append([]s3Request(nil), seen...)
	}
}

// Every bucket call is path style, carries the subresource its URI names
// as a query parameter, and sends the payload hash S3 requires; each
// mapped feature reads into the schema's shape.
func TestReadS3Bucket(t *testing.T) {
	client, requests := s3Client(t, func(query string) (int, string) {
		if code, unset := s3Unset[query]; unset {
			return s3NotFound(code)
		}
		return 200, s3Bodies[query]
	})
	got, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "my.bucket/x"})
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(`{
		"BucketName": "my.bucket/x",
		"Arn": "arn:aws:s3:::my.bucket/x",
		"DomainName": "my.bucket/x.s3.amazonaws.com",
		"RegionalDomainName": "my.bucket/x.s3.eu-west-1.amazonaws.com",
		"DualStackDomainName": "my.bucket/x.s3.dualstack.eu-west-1.amazonaws.com",
		"WebsiteURL": "http://my.bucket/x.s3-website-eu-west-1.amazonaws.com",
		"VersioningConfiguration": {"Status": "Enabled"},
		"AccelerateConfiguration": {"AccelerationStatus": "Suspended"},
		"AbacStatus": "Enabled",
		"BucketEncryption": {"ServerSideEncryptionConfiguration": [{
			"ServerSideEncryptionByDefault": {"SSEAlgorithm": "aws:kms", "KMSMasterKeyID": "key-1"},
			"BucketKeyEnabled": true,
			"BlockedEncryptionTypes": {"EncryptionType": ["SSE-C"]}}]},
		"OwnershipControls": {"Rules": [{"ObjectOwnership": "BucketOwnerPreferred"}]},
		"PublicAccessBlockConfiguration": {"BlockPublicAcls": true, "IgnorePublicAcls": false, "BlockPublicPolicy": true, "RestrictPublicBuckets": false},
		"Tags": [{"Key": "team", "Value": "core"}, {"Key": "env", "Value": "dev"}],
		"LifecycleConfiguration": {"TransitionDefaultMinimumObjectSize": "all_storage_classes_128K", "Rules": [
			{"Id": "r1", "Status": "Enabled", "ExpirationInDays": 30, "ObjectSizeGreaterThan": "1024", "TagFilters": [{"Key": "d", "Value": "4"}], "Prefix": "tmp/"},
			{"Id": "r2", "Status": "Enabled", "Transitions": [{"StorageClass": "STANDARD_IA", "TransitionInDays": 40}], "NoncurrentVersionExpiration": {"NoncurrentDays": 7}, "Prefix": "old/"}]},
		"NotificationConfiguration": {
			"EventBridgeConfiguration": {"EventBridgeEnabled": true},
			"TopicConfigurations": [
				{"Event": "s3:ObjectCreated:*", "Topic": "arn:aws:sns:eu-west-1:111122223333:t", "Filter": {"S3Key": {"Rules": [{"Name": "Prefix", "Value": "in/"}, {"Name": "Suffix", "Value": ".csv"}]}}},
				{"Event": "s3:ObjectRemoved:*", "Topic": "arn:aws:sns:eu-west-1:111122223333:t", "Filter": {"S3Key": {"Rules": [{"Name": "Prefix", "Value": "in/"}, {"Name": "Suffix", "Value": ".csv"}]}}}],
			"QueueConfigurations": [{"Event": "s3:ObjectRestore:Completed", "Queue": "arn:aws:sqs:eu-west-1:111122223333:q"}]},
		"MetadataConfiguration": {
			"Destination":               {"TableBucketType": "aws", "TableBucketArn": "arn:aws:s3tables:eu-west-1:111122223333:bucket/aws-s3", "TableNamespace": "b_ns"},
			"JournalTableConfiguration":   {"TableName": "journal", "TableArn": "arn:aws:s3tables:eu-west-1:111122223333:bucket/aws-s3/table/j", "RecordExpiration": {"Expiration": "ENABLED", "Days": 7}},
			"InventoryTableConfiguration": {"ConfigurationState": "DISABLED"}},
		"MetricsConfigurations": [
			{"Id": "m1", "Prefix": "data/", "TagFilters": [{"Key": "c", "Value": "3"}]},
			{"Id": "m2", "TagFilters": [{"Key": "a", "Value": "1"}]},
			{"Id": "m3", "AccessPointArn": "arn:aws:s3:eu-west-1:111122223333:accesspoint/ap"}],
		"AnalyticsConfigurations": [{"Id": "an1", "Prefix": "logs/", "TagFilters": [{"Key": "a", "Value": "1"}, {"Key": "b", "Value": "2"}], "StorageClassAnalysis": {}}],
		"IntelligentTieringConfigurations": [{"Id": "it1", "Prefix": "cold/", "Status": "Enabled", "Tierings": [{"AccessTier": "ARCHIVE_ACCESS", "Days": 90}]}],
		"LoggingConfiguration": {"DestinationBucketName": "logs", "LogFilePrefix": "b/",
			"TargetObjectKeyFormat": {"PartitionedPrefix": {"PartitionDateSource": "EventTime"}}},
		"CorsConfiguration": {"CorsRules": [{"Id": "web", "AllowedHeaders": ["*", "x-amz-meta-a"], "AllowedMethods": ["GET"],
			"AllowedOrigins": ["https://example.com"], "ExposedHeaders": ["ETag"], "MaxAge": 3000}]}
	}`), &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	var round map[string]any
	if err := json.Unmarshal(gotJSON, &round); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round, want) {
		t.Fatalf("Read = %s\nwant   %s", gotJSON, mustJSON(t, want))
	}

	empty := sha256.Sum256(nil)
	wantQueries := []string{"abac=", "accelerate=", "analytics=&x-id=ListBucketAnalyticsConfigurations", "cors=", "encryption=", "intelligent-tiering=&x-id=ListBucketIntelligentTieringConfigurations", "inventory=&x-id=ListBucketInventoryConfigurations", "lifecycle=", "logging=", "metadataConfiguration=", "metadataTable=", "metrics=&x-id=ListBucketMetricsConfigurations", "notification=", "object-lock=", "ownershipControls=", "publicAccessBlock=", "replication=", "tagging=", "versioning=", "website="}
	var queries []string
	for _, r := range requests() {
		queries = append(queries, r.query)
		if r.method != "GET" || r.path != "/my.bucket%2Fx" {
			t.Errorf("%s: %s %s, want GET /my.bucket%%2Fx", r.query, r.method, r.path)
		}
		if r.sha != hex.EncodeToString(empty[:]) {
			t.Errorf("%s: X-Amz-Content-Sha256 = %q", r.query, r.sha)
		}
		if !strings.Contains(r.auth, "/eu-west-1/s3/aws4_request") || !strings.Contains(r.auth, "x-amz-content-sha256") {
			t.Errorf("%s: Authorization = %q, want the s3 scope in eu-west-1 signing the payload hash header", r.query, r.auth)
		}
	}
	if !reflect.DeepEqual(queries, wantQueries) {
		t.Fatalf("queries = %v, want %v", queries, wantQueries)
	}
}

// A URI's literal query joins the bound query values in one query string:
// the S3 list operations' ?analytics&x-id=... beside a continuation token.
func TestQueryLiteralJoinsBoundQuery(t *testing.T) {
	client := &Client{
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""), Region: "us-east-1",
		Now: func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) },
	}
	r := Reader{Protocol: "restXml", SigningName: "s3", Host: "s3.{region}.amazonaws.com"}
	req, err := client.request(context.Background(), r, "GET", "/{Bucket}?analytics&x-id=ListBucketAnalyticsConfigurations", "", []Binding{
		{Member: "Bucket", Location: "label", Value: "b"},
		{Name: "continuation-token", Location: "query", Value: "t"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := req.URL.String(), "https://s3.us-east-1.amazonaws.com/b?analytics=&continuation-token=t&x-id=ListBucketAnalyticsConfigurations"; got != want {
		t.Fatalf("URL = %s, want %s", got, want)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A feature the bucket never set answers with its own not-found code and
// leaves the property out; only the bucket's own absence is absence.
// s3NotFound is S3's answer for a configuration a bucket does not have.
func s3NotFound(code string) (int, string) {
	return 404, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>m</Message></Error>`
}

// s3Unset is the configurations TestReadS3Bucket's bucket does not have,
// by subresource, and the code S3 answers for each.
// s3Headers is the response headers S3 answers a subresource with,
// beside its body.
var s3Headers = map[string]map[string]string{
	"lifecycle": {"x-amz-transition-default-minimum-object-size": "all_storage_classes_128K"},
}

var s3Unset = map[string]string{
	"metadataTable": "V1APIsNotAllowed",
	"object-lock":   "ObjectLockConfigurationNotFoundError",
	"replication":   "ReplicationConfigurationNotFoundError",
	"website":       "NoSuchWebsiteConfiguration",
}

func TestReadS3BucketAbsence(t *testing.T) {
	notFound := s3NotFound
	client, _ := s3Client(t, func(query string) (int, string) {
		switch query {
		case "cors":
			return notFound("NoSuchCORSConfiguration")
		case "tagging":
			return notFound("NoSuchTagSet")
		case "ownershipControls":
			return notFound("OwnershipControlsNotFoundError")
		case "publicAccessBlock":
			return notFound("NoSuchPublicAccessBlockConfiguration")
		case "lifecycle":
			return notFound("NoSuchLifecycleConfiguration")
		case "metadataConfiguration":
			return notFound("MetadataConfigurationNotFound")
		}
		if code, unset := s3Unset[query]; unset {
			return notFound(code)
		}
		return 200, s3Bodies[query]
	})
	got, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, property := range []string{"CorsConfiguration", "Tags", "OwnershipControls", "PublicAccessBlockConfiguration", "ObjectLockConfiguration", "ReplicationConfiguration", "WebsiteConfiguration", "LifecycleConfiguration", "MetadataConfiguration", "MetadataTableConfiguration"} {
		if v, present := got[property]; present {
			t.Errorf("%s = %v, want it left out", property, v)
		}
	}
	if got["VersioningConfiguration"] == nil || got["AbacStatus"] != "Enabled" {
		t.Errorf("Read = %v, want the features the bucket sets", got)
	}

	gone, _ := s3Client(t, func(string) (int, string) { return 404, `<Error><Code>NoSuchBucket</Code><Message>m</Message></Error>` })
	if _, err := gone.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "b"}); !errors.Is(err, ErrAbsent) {
		t.Fatalf("a missing bucket read %v, want absent", err)
	}
}

// S3 signs the path as sent: with disableDoubleEncoding, a bucket name
// whose escaping matters signs differently than it would escaped twice,
// which would be a signature S3 refuses.
func TestS3SignsThePathAsSent(t *testing.T) {
	saved := readers["AWS::S3::Bucket"]
	if !saved.DisableDoubleEncoding {
		t.Fatal("the S3 reader does not sign the path as sent")
	}
	defer func() { readers["AWS::S3::Bucket"] = saved }()
	client, requests := s3Client(t, func(query string) (int, string) {
		if code, unset := s3Unset[query]; unset {
			return s3NotFound(code)
		}
		return 200, s3Bodies[query]
	})
	for _, asSent := range []bool{true, false} {
		r := saved
		r.DisableDoubleEncoding = asSent
		readers["AWS::S3::Bucket"] = r
		if _, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "my.bucket/x"}); err != nil {
			t.Fatal(err)
		}
	}
	// Same client, host, clock and path: only the escaping can differ.
	signatures := map[string]bool{}
	for _, req := range requests() {
		if req.query == "versioning=" {
			signatures[req.auth] = true
		}
	}
	if len(signatures) != 2 {
		t.Fatalf("%d distinct signatures of the two reads, want 2: the signature ignores disableDoubleEncoding", len(signatures))
	}
}

// An S3 override that leaves the bucket unbound would be resolved to the
// directory-bucket control host, the trap #443 found; one bound to a
// member the operation does not name, or without path style, is refused
// too.
func TestS3EndpointParamsAreChecked(t *testing.T) {
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := loadLock(files)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(o Override) bool { return o.Type == "AWS::S3::Bucket" })
	if i < 0 {
		t.Fatal("no S3 bucket override")
	}
	for name, c := range map[string]struct {
		params  map[string]any
		refused string
	}{
		"the bucket unbound":        {map[string]any{"ForcePathStyle": true}, "signs https://s3express-control.us-kraai-1.amazonaws.com for s3express, not s3"},
		"a member it does not name": {map[string]any{"ForcePathStyle": true, "Bucket": "{Nope}"}, "GetBucketVersioning does not name Nope as the context parameter Bucket, which endpointParams binds it to"},
		"virtual-hosted addressing": {map[string]any{"Bucket": "{Bucket}"}, "whose path is not the bound parameter alone"},
		"two members bound":         {map[string]any{"ForcePathStyle": true, "Bucket": "{Bucket}", "Key": "{Key}"}, "binds 2 parameters"},
	} {
		t.Run(name, func(t *testing.T) {
			o := all[i]
			o.EndpointParams = c.params
			_, errs := compileOne(withDecoded(files), lock, o)
			if got := fmt.Sprint(errs); !strings.Contains(got, c.refused) {
				t.Fatalf("errors = %s, want %q", got, c.refused)
			}
		})
	}
}

// A template's form follows the region: S3's website endpoint keeps a dash
// in the oldest regions and takes a dot in the rest.
func TestTemplateRegions(t *testing.T) {
	r := readers["AWS::S3::Bucket"]
	i := slices.IndexFunc(r.Fields, func(f Field) bool { return f.Property == "WebsiteURL" })
	if i < 0 {
		t.Fatal("no WebsiteURL field")
	}
	for region, want := range map[string]string{
		"us-east-1": "http://b.s3-website-us-east-1.amazonaws.com",
		"us-east-2": "http://b.s3-website.us-east-2.amazonaws.com",
		"eu-west-1": "http://b.s3-website-eu-west-1.amazonaws.com",
		"eu-west-2": "http://b.s3-website.eu-west-2.amazonaws.com",
	} {
		got := r.translate(&walk{vars: map[string]string{"region": region, "BucketName": "b"}}, map[string]any{}, r.Fields[i:i+1])
		if got["WebsiteURL"] != want {
			t.Errorf("%s: WebsiteURL = %v, want %s", region, got["WebsiteURL"], want)
		}
	}
}

// inventoryPage is one page of ListBucketInventoryConfigurations: one
// configuration, and the next page's token when there is one.
func inventoryPage(id, next string) string {
	truncation := "<IsTruncated>false</IsTruncated>"
	if next != "" {
		truncation = "<IsTruncated>true</IsTruncated><NextContinuationToken>" + next + "</NextContinuationToken>"
	}
	return `<ListInventoryConfigurationsResult` + s3NS + `><InventoryConfiguration><Id>` + id + `</Id><IsEnabled>true</IsEnabled>` +
		`<Destination><S3BucketDestination><AccountId>111122223333</AccountId><Bucket>arn:aws:s3:::dest</Bucket><Format>CSV</Format></S3BucketDestination></Destination>` +
		`<Filter><Prefix>` + id + `/</Prefix></Filter><IncludedObjectVersions>All</IncludedObjectVersions><Schedule><Frequency>Daily</Frequency></Schedule></InventoryConfiguration>` +
		truncation + `</ListInventoryConfigurationsResult>`
}

// pagedS3Client answers every bucket call as TestReadS3Bucket's does,
// and the inventory list from pages by the continuation token asked for.
func pagedS3Client(t *testing.T, pages map[string]string) (*Client, func() []s3Request) {
	t.Helper()
	var mu sync.Mutex
	var seen []s3Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, s3Request{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, "", ""})
		mu.Unlock()
		q := r.URL.Query()
		if q.Has("inventory") {
			_, _ = io.WriteString(w, pages[q.Get("continuation-token")])
			return
		}
		first := strings.TrimSuffix(strings.SplitN(r.URL.RawQuery, "&", 2)[0], "=")
		if code, unset := s3Unset[first]; unset {
			status, body := s3NotFound(code)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, s3Bodies[first])
	}))
	t.Cleanup(srv.Close)
	client := &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "eu-west-1", RetryDelay: time.Millisecond,
		Endpoint: func(string) string { return srv.URL },
		Now:      func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) },
	}
	return client, func() []s3Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]s3Request(nil), seen...)
	}
}

// A declared page token is followed: each page's configurations join one
// list, and the next call sends the token the last answer gave.
func TestReadFollowsDeclaredPages(t *testing.T) {
	client, requests := pagedS3Client(t, map[string]string{"": inventoryPage("first", "t1"), "t1": inventoryPage("second", "")})
	got, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "b"})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := got["InventoryConfigurations"].([]any)
	var ids []any
	for _, c := range list {
		ids = append(ids, c.(map[string]any)["Id"])
	}
	if !reflect.DeepEqual(ids, []any{"first", "second"}) {
		t.Fatalf("InventoryConfigurations ids = %v, want [first second]; read %v", ids, got["InventoryConfigurations"])
	}
	want := map[string]any{"Id": "first", "Enabled": true, "IncludedObjectVersions": "All", "Prefix": "first/", "ScheduleFrequency": "Daily",
		"Destination": map[string]any{"BucketAccountId": "111122223333", "BucketArn": "arn:aws:s3:::dest", "Format": "CSV"}}
	if !reflect.DeepEqual(list[0], want) {
		t.Fatalf("first configuration = %v, want %v", list[0], want)
	}
	var tokens []string
	for _, r := range requests() {
		if v, _ := url.ParseQuery(r.query); v.Has("inventory") {
			tokens = append(tokens, v.Get("continuation-token"))
		}
	}
	if !reflect.DeepEqual(tokens, []string{"", "t1"}) {
		t.Fatalf("inventory calls sent tokens %q, want none then t1", tokens)
	}
}

// A page that answers a token already followed is refused, not read
// forever.
func TestReadRefusesARepeatedPageToken(t *testing.T) {
	client, requests := pagedS3Client(t, map[string]string{"": inventoryPage("first", "t1"), "t1": inventoryPage("second", "t1")})
	_, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "b"})
	if err == nil || !strings.Contains(err.Error(), `answered page token "t1" again`) {
		t.Fatalf("Read = %v, want the repeated token refused", err)
	}
	calls := 0
	for _, r := range requests() {
		if v, _ := url.ParseQuery(r.query); v.Has("inventory") {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("%d inventory calls, want 2: the second answer repeats the token", calls)
	}
}
