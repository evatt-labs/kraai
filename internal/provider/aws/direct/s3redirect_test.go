package direct

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// s3Elsewhere is a bucket that lives in region: S3 answers any request
// signed for another region 301 PermanentRedirect, naming region in
// X-Amz-Bucket-Region as it did live from us-east-1 for a us-east-2 bucket,
// and answers the rest as s3Client's server does. names is the region the
// redirect names, region unless set.
type s3Elsewhere struct {
	region, names string
	header        bool

	mu                sync.Mutex
	hosts             map[string]int
	redirects, signed int
	// writes is each PUT and DELETE answered in region, as METHOD query.
	writes  []string
	deleted bool
}

func (s *s3Elsewhere) client(t *testing.T) *Client {
	t.Helper()
	s.hosts = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		auth := r.Header.Get("Authorization")
		here := strings.Contains(auth, "/"+s.region+"/s3/aws4_request")
		s.mu.Lock()
		s.signed++
		if !here {
			s.redirects++
		}
		s.mu.Unlock()
		if !here {
			if s.header {
				names := s.names
				if names == "" {
					names = s.region
				}
				w.Header().Set("X-Amz-Bucket-Region", names)
			}
			w.WriteHeader(http.StatusMovedPermanently)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>PermanentRedirect</Code><Message>The bucket you are attempting to access must be addressed using the specified endpoint.</Message></Error>`)
			return
		}
		query := strings.TrimSuffix(strings.SplitN(r.URL.RawQuery, "&", 2)[0], "=")
		s.mu.Lock()
		if r.Method != http.MethodGet {
			s.writes = append(s.writes, r.Method+" "+query)
			s.deleted = s.deleted || r.Method == http.MethodDelete && query == ""
		}
		deleted := s.deleted
		s.mu.Unlock()
		switch {
		case r.Method != http.MethodGet:
			w.WriteHeader(http.StatusNoContent)
			return
		case deleted:
			status, body := s3NotFound("NoSuchBucket")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		if code, unset := s3Unset[query]; unset {
			status, body := s3NotFound(code)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		for k, v := range s3Headers[query] {
			w.Header().Set(k, v)
		}
		_, _ = io.WriteString(w, s3Bodies[query])
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", RetryDelay: time.Millisecond,
		Endpoint: func(host string) string {
			s.mu.Lock()
			s.hosts[host]++
			s.mu.Unlock()
			return srv.URL
		},
		Now: func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) },
	}
}

// A bucket in another region is read there: the first call's redirect
// is followed once, every further call goes to that region's endpoint
// signed for it, and the domain names are built from the bucket's region,
// as Cloud Control builds them, not the client's.
func TestReadS3BucketInAnotherRegion(t *testing.T) {
	s := &s3Elsewhere{region: "us-east-2", header: true}
	client := s.client(t)
	got, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "b"})
	if err != nil {
		t.Fatal(err)
	}
	for property, want := range map[string]string{
		"RegionalDomainName":  "b.s3.us-east-2.amazonaws.com",
		"DualStackDomainName": "b.s3.dualstack.us-east-2.amazonaws.com",
		// us-east-2 takes the dotted website form; the client's us-east-1
		// would have given the dashed one.
		"WebsiteURL": "http://b.s3-website.us-east-2.amazonaws.com",
		"DomainName": "b.s3.amazonaws.com",
	} {
		if got[property] != want {
			t.Errorf("%s = %v, want %s", property, got[property], want)
		}
	}
	if s.redirects != 1 {
		t.Errorf("%d requests were redirected, want only the first", s.redirects)
	}
	if s.hosts["s3.us-east-1.amazonaws.com"] != 1 || s.hosts["s3.us-east-2.amazonaws.com"] != s.signed-1 {
		t.Errorf("hosts = %v over %d requests, want one to us-east-1 and the rest to us-east-2", s.hosts, s.signed)
	}
	if client.Region != "us-east-1" {
		t.Errorf("the client's region became %s", client.Region)
	}
}

// A redirect is followed only to a region of the client's partition named
// as a region is, and only once; otherwise the read fails as it did, and
// Cloud Control reads the bucket instead.
func TestS3RedirectNotFollowed(t *testing.T) {
	for name, tc := range map[string]struct {
		s *s3Elsewhere
		// requests is how many are sent: the read alone when the
		// redirect is refused, the read and one follow when the region
		// it names answers 301 again.
		requests int
	}{
		"no region header":     {&s3Elsewhere{region: "us-east-2"}, 1},
		"a host, not a region": {&s3Elsewhere{region: "us-east-2", header: true, names: "evil.example/x"}, 1},
		"another partition":    {&s3Elsewhere{region: "us-east-2", header: true, names: "cn-north-1"}, 1},
		"back to the client's": {&s3Elsewhere{region: "us-east-2", header: true, names: "us-east-1"}, 1},
		"onward again":         {&s3Elsewhere{region: "us-east-2", header: true, names: "eu-west-3"}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			s := tc.s
			client := s.client(t)
			_, err := client.Read(context.Background(), "AWS::S3::Bucket", map[string]string{"BucketName": "b"})
			var api *APIError
			if !errors.As(err, &api) || api.Status != http.StatusMovedPermanently || errors.Is(err, ErrAbsent) {
				t.Fatalf("Read = %v, want the 301 refused", err)
			}
			if s.signed != tc.requests {
				t.Errorf("%d requests to %v, want %d", s.signed, s.hosts, tc.requests)
			}
		})
	}
}

// An update or delete of a bucket in another region is made there: its
// first call's redirect is followed once, and every write and the wait
// after it go to the bucket's region.
func TestMutateS3BucketInAnotherRegion(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Client) error
		writes []string
	}{
		"update": {
			mutate: func(c *Client) error {
				return c.Update(context.Background(), "AWS::S3::Bucket", "b",
					map[string]any{"VersioningConfiguration": map[string]any{"Status": "Suspended"}},
					map[string]any{"VersioningConfiguration": map[string]any{"Status": "Enabled"}})
			},
			writes: []string{"PUT versioning"},
		},
		"delete": {
			mutate: func(c *Client) error { return c.Delete(context.Background(), "AWS::S3::Bucket", "b") },
			writes: []string{"DELETE "},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := &s3Elsewhere{region: "us-east-2", header: true}
			client := s.client(t)
			client.Wait, client.Poll = time.Second, time.Millisecond
			if err := tc.mutate(client); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(s.writes, tc.writes) {
				t.Errorf("writes in us-east-2 = %v, want %v", s.writes, tc.writes)
			}
			if s.redirects != 1 {
				t.Errorf("%d requests were redirected, want only the first", s.redirects)
			}
		})
	}
}
