package direct

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// s3Write is one request a mutation test's fake answered.
type s3Write struct {
	method, path, query, body string
	header                    http.Header
}

// s3MutationClient answers every read as TestReadS3Bucket's fake does and
// records every other request, answering it empty.
func s3MutationClient(t *testing.T, region string) (*Client, func() []s3Write) {
	t.Helper()
	var mu sync.Mutex
	var writes []s3Write
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := strings.TrimSuffix(strings.SplitN(r.URL.RawQuery, "&", 2)[0], "=")
		if r.Method != http.MethodGet {
			mu.Lock()
			writes = append(writes, s3Write{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body), r.Header.Clone()})
			mu.Unlock()
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
	client := &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: region, RetryDelay: time.Millisecond, Poll: time.Millisecond, Wait: time.Second,
		Endpoint: func(host string) string {
			if want := "s3." + region + ".amazonaws.com"; host != want {
				t.Errorf("request host %q, want %s", host, want)
			}
			return srv.URL
		},
	}
	return client, func() []s3Write {
		mu.Lock()
		defer mu.Unlock()
		return append([]s3Write(nil), writes...)
	}
}

// A bucket made in us-east-1 names no location; anywhere else the create
// carries it, and object lock is asked for in its header.
func TestCreateS3BucketNamesItsLocation(t *testing.T) {
	for region, want := range map[string]string{
		"us-east-1": "",
		"eu-west-1": `<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LocationConstraint>eu-west-1</LocationConstraint></CreateBucketConfiguration>`,
	} {
		t.Run(region, func(t *testing.T) {
			client, writes := s3MutationClient(t, region)
			// The fake's reads never show object lock on, so the wait gives
			// up; the create's request is what is checked.
			_, err := client.Create(context.Background(), "AWS::S3::Bucket", map[string]any{"BucketName": "my-bucket", "ObjectLockEnabled": true})
			if err == nil || !strings.Contains(err.Error(), "not visible") {
				t.Fatalf("Create = %v, want only the wait to give up", err)
			}
			w := writes()
			if len(w) != 1 || w[0].method != http.MethodPut || w[0].path != "/my-bucket" || w[0].query != "" {
				t.Fatalf("writes = %+v, want one PUT /my-bucket", w)
			}
			if w[0].body != want {
				t.Fatalf("create body =\n%s\nwant\n%s", w[0].body, want)
			}
			if got := w[0].header.Get("X-Amz-Bucket-Object-Lock-Enabled"); got != "true" {
				t.Fatalf("object lock header = %q, want true", got)
			}
		})
	}
}

// An update puts each changed configuration whole, in the S3 namespace,
// with the CRC32 of its body; the website's two documents are rebuilt from
// the key and suffix the schema flattens them to.
func TestUpdateS3BucketPutsEachConfiguration(t *testing.T) {
	client, writes := s3MutationClient(t, "us-east-1")
	changes := map[string]any{
		"Tags":                 []any{map[string]any{"Key": "team", "Value": "kraai"}},
		"WebsiteConfiguration": map[string]any{"IndexDocument": "index.html", "ErrorDocument": "error.html"},
	}
	err := client.Update(context.Background(), "AWS::S3::Bucket", "my-bucket", map[string]any{"BucketName": "my-bucket"}, changes)
	// The fake's reads never show the change, so the wait gives up; the
	// calls are what is checked.
	if err == nil || !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("Update = %v, want only the wait to give up", err)
	}
	ns := ` xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`
	want := map[string]string{
		"tagging=": `<Tagging` + ns + `><TagSet><Tag><Key>team</Key><Value>kraai</Value></Tag></TagSet></Tagging>`,
		"website=": `<WebsiteConfiguration` + ns + `><ErrorDocument><Key>error.html</Key></ErrorDocument><IndexDocument><Suffix>index.html</Suffix></IndexDocument></WebsiteConfiguration>`,
	}
	got := map[string]bool{}
	for _, w := range writes() {
		body, ok := want[w.query]
		if !ok {
			t.Errorf("unexpected %s %s?%s", w.method, w.path, w.query)
			continue
		}
		got[w.query] = true
		if w.method != http.MethodPut || w.path != "/my-bucket" || w.body != body {
			t.Errorf("%s: %s %s body\n%s\nwant\n%s", w.query, w.method, w.path, w.body, body)
		}
		sum := make([]byte, 4)
		binary.BigEndian.PutUint32(sum, crc32.ChecksumIEEE([]byte(w.body)))
		if w.header.Get("X-Amz-Checksum-Crc32") != base64.StdEncoding.EncodeToString(sum) || w.header.Get("Content-Type") != "application/xml" {
			t.Errorf("%s: checksum %q, content type %q", w.query, w.header.Get("X-Amz-Checksum-Crc32"), w.header.Get("Content-Type"))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("puts %v, want %v", got, want)
	}
}
