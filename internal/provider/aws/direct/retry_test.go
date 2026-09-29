package direct

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// flakyQueue answers SQS as a queue that exists, failing the first fails
// requests of each action by answer: "drop" closes the connection with no
// response, as a reset or EOF does, and a status code answers with it.
type flakyQueue struct {
	mu       sync.Mutex
	fails    map[string]int
	answer   string
	status   int
	attempts map[string]int
}

func (f *flakyQueue) serve(t *testing.T) *Client {
	t.Helper()
	f.attempts = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		op := r.Header.Get("X-Amz-Target")
		f.mu.Lock()
		f.attempts[op]++
		fail := f.attempts[op] <= f.fails[op]
		f.mu.Unlock()
		if fail {
			switch f.answer {
			case "drop":
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			case "throttle":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"__type":"com.amazonaws.sqs#ThrottlingException","message":"slow down"}`)
			default:
				w.WriteHeader(f.status)
				_, _ = io.WriteString(w, `{"__type":"InternalError","message":"oops"}`)
			}
			return
		}
		switch op {
		case "AmazonSQS.CreateQueue":
			_, _ = io.WriteString(w, `{"QueueUrl":"https://sqs.us-east-1.amazonaws.com/1/kraai-q"}`)
		case "AmazonSQS.GetQueueAttributes":
			_, _ = io.WriteString(w, `{"Attributes":{"QueueArn":"arn:aws:sqs:us-east-1:1:kraai-q","DelaySeconds":"5"}}`)
		case "AmazonSQS.ListQueueTags":
			_, _ = io.WriteString(w, `{"Tags":{"kraai:resource-name":"kraai-q"}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL },
		Wait: time.Second, Poll: time.Millisecond, RetryDelay: time.Millisecond}
}

// count is how many requests of op the queue has seen.
func (f *flakyQueue) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[op]
}

const queueURL = "https://sqs.us-east-1.amazonaws.com/1/kraai-q"

// A read that loses its connection, or is answered with a 5xx, is sent
// again, up to the SDK's three attempts.
func TestReadRetriesTransientFailures(t *testing.T) {
	for name, f := range map[string]*flakyQueue{
		"a dropped connection": {answer: "drop", fails: map[string]int{"AmazonSQS.GetQueueAttributes": 2}},
		"a 503":                {status: http.StatusServiceUnavailable, fails: map[string]int{"AmazonSQS.GetQueueAttributes": 2}},
	} {
		t.Run(name, func(t *testing.T) {
			client := f.serve(t)
			if _, err := client.ReadByID(context.Background(), "AWS::SQS::Queue", queueURL); err != nil {
				t.Fatalf("ReadByID = %v, want it to succeed on the third attempt", err)
			}
			if n := f.count("AmazonSQS.GetQueueAttributes"); n != 3 {
				t.Fatalf("attempts = %d, want 3", n)
			}
		})
	}
}

// Three failures in a row are the SDK's limit: the last one is returned.
func TestReadGivesUpAfterThreeAttempts(t *testing.T) {
	f := &flakyQueue{answer: "drop", fails: map[string]int{"AmazonSQS.GetQueueAttributes": 5}}
	client := f.serve(t)
	_, err := client.ReadByID(context.Background(), "AWS::SQS::Queue", queueURL)
	var sent *sendError
	if !errors.As(err, &sent) {
		t.Fatalf("ReadByID = %v, want the send failure", err)
	}
	if n := f.count("AmazonSQS.GetQueueAttributes"); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

// A refusal that is not transient is returned at once.
func TestReadDoesNotRetryARefusal(t *testing.T) {
	f := &flakyQueue{status: http.StatusBadRequest, fails: map[string]int{"AmazonSQS.GetQueueAttributes": 5}}
	client := f.serve(t)
	if _, err := client.ReadByID(context.Background(), "AWS::SQS::Queue", queueURL); err == nil {
		t.Fatal("ReadByID succeeded")
	}
	if n := f.count("AmazonSQS.GetQueueAttributes"); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}

// An update sent again after it took effect sets the same value, so a
// dropped connection is retried.
func TestUpdateRetriesADroppedConnection(t *testing.T) {
	f := &flakyQueue{answer: "drop", fails: map[string]int{"AmazonSQS.SetQueueAttributes": 1}}
	client := f.serve(t)
	current := map[string]any{"QueueUrl": queueURL, "DelaySeconds": 0}
	if err := client.Update(context.Background(), "AWS::SQS::Queue", queueURL, current, map[string]any{"DelaySeconds": 5}); err != nil {
		t.Fatal(err)
	}
	if n := f.count("AmazonSQS.SetQueueAttributes"); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
}

// A cancelled context is never retried.
func TestRetryStopsOnCancel(t *testing.T) {
	f := &flakyQueue{answer: "drop", fails: map[string]int{"AmazonSQS.GetQueueAttributes": 5}}
	client := f.serve(t)
	client.RetryDelay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.ReadByID(ctx, "AWS::SQS::Queue", queueURL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadByID = %v, want the deadline", err)
	}
}

// A sendError is what the SDK's classifier recognizes: the wrapper, not a
// bare EOF, is what makes a lost response retryable.
func TestSendErrorIsAConnectionError(t *testing.T) {
	bare := &url.Error{Op: "Post", URL: "https://x", Err: io.EOF}
	if retryTransient.allows(bare) {
		t.Fatal("a bare EOF is retryable; the wrapper would be untested")
	}
	if !retryTransient.allows(&sendError{bare}) {
		t.Fatal("a sendError is not retryable")
	}
}
