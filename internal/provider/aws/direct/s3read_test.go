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
	"versioning":        `<VersioningConfiguration` + s3NS + `><Status>Enabled</Status></VersioningConfiguration>`,
	"accelerate":        `<AccelerateConfiguration` + s3NS + `><Status>Suspended</Status></AccelerateConfiguration>`,
	"abac":              `<AbacStatus` + s3NS + `><Status>Enabled</Status></AbacStatus>`,
	"encryption":        `<ServerSideEncryptionConfiguration` + s3NS + `><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>key-1</KMSMasterKeyID></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>true</BucketKeyEnabled><BlockedEncryptionTypes><EncryptionType>SSE-C</EncryptionType></BlockedEncryptionTypes></Rule></ServerSideEncryptionConfiguration>`,
	"ownershipControls": `<OwnershipControls` + s3NS + `><Rule><ObjectOwnership>BucketOwnerPreferred</ObjectOwnership></Rule></OwnershipControls>`,
	"publicAccessBlock": `<PublicAccessBlockConfiguration` + s3NS + `><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>false</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>false</RestrictPublicBuckets></PublicAccessBlockConfiguration>`,
	"tagging":           `<Tagging` + s3NS + `><TagSet><Tag><Key>team</Key><Value>core</Value></Tag><Tag><Key>env</Key><Value>dev</Value></Tag></TagSet></Tagging>`,
	"logging":           `<BucketLoggingStatus` + s3NS + `><LoggingEnabled><TargetBucket>logs</TargetBucket><TargetPrefix>b/</TargetPrefix><TargetObjectKeyFormat><PartitionedPrefix><PartitionDateSource>EventTime</PartitionDateSource></PartitionedPrefix></TargetObjectKeyFormat></LoggingEnabled></BucketLoggingStatus>`,
	"cors":              `<CORSConfiguration` + s3NS + `><CORSRule><ID>web</ID><AllowedHeader>*</AllowedHeader><AllowedHeader>x-amz-meta-a</AllowedHeader><AllowedMethod>GET</AllowedMethod><AllowedOrigin>https://example.com</AllowedOrigin><ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>3000</MaxAgeSeconds></CORSRule></CORSConfiguration>`,
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
		status, body := answer(strings.TrimSuffix(strings.SplitN(r.URL.RawQuery, "&", 2)[0], "="))
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
	wantQueries := []string{"abac=", "accelerate=", "cors=", "encryption=", "logging=", "object-lock=", "ownershipControls=", "publicAccessBlock=", "replication=", "tagging=", "versioning=", "website="}
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
var s3Unset = map[string]string{
	"object-lock": "ObjectLockConfigurationNotFoundError",
	"replication": "ReplicationConfigurationNotFoundError",
	"website":     "NoSuchWebsiteConfiguration",
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
	for _, property := range []string{"CorsConfiguration", "Tags", "OwnershipControls", "PublicAccessBlockConfiguration", "ObjectLockConfiguration", "ReplicationConfiguration", "WebsiteConfiguration"} {
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
		"a member it does not name": {map[string]any{"ForcePathStyle": true, "Bucket": "{Nope}"}, "binds Bucket to Nope, which GetBucketVersioning does not name as that context parameter"},
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
