package direct

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// lossyEC2 sits in front of a fake EC2 endpoint and, for the first fails
// requests of action, either forwards the request and drops the response
// (mode "drop": the service acted, the caller never heard) or answers a
// throttle without forwarding it (mode "throttle"). It records the form
// of every request of action.
type lossyEC2 struct {
	mu     sync.Mutex
	action string
	mode   string
	fails  int
	forms  []url.Values
}

func (l *lossyEC2) front(t *testing.T, client *Client) {
	t.Helper()
	backend := client.Endpoint("")
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		l.mu.Lock()
		lose := false
		if form.Get("Action") == l.action {
			l.forms = append(l.forms, form)
			lose = len(l.forms) <= l.fails
		}
		l.mu.Unlock()
		if lose && l.mode == "throttle" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `<Response><Errors><Error><Code>RequestLimitExceeded</Code><Message>slow down</Message></Error></Errors></Response>`)
			return
		}
		resp, err := client.HTTP.Post(backend, r.Header.Get("Content-Type"), strings.NewReader(string(raw)))
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if lose {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(front.Close)
	client.Endpoint = func(string) string { return front.URL }
	client.RetryDelay = 1
}

func (l *lossyEC2) attempts() []url.Values {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]url.Values(nil), l.forms...)
}

// A create with an idempotency token sends one token on every attempt of
// one logical create, and is sent again after a lost response.
func TestCreateSendsOneTokenAcrossAttempts(t *testing.T) {
	f := &fakeRouteTables{}
	client := f.serve(t)
	lossy := &lossyEC2{action: "CreateRouteTable", mode: "drop", fails: 2}
	lossy.front(t, client)
	if _, err := client.Create(context.Background(), routeTable, map[string]any{"VpcId": "vpc-1", "Tags": []any{routeTableNameTag}}); err != nil {
		t.Fatalf("Create = %v, want it to succeed on the third attempt", err)
	}
	sent := lossy.attempts()
	if len(sent) != 3 {
		t.Fatalf("attempts = %d, want 3", len(sent))
	}
	token := sent[0].Get("ClientToken")
	if token == "" {
		t.Fatalf("first attempt sent no ClientToken: %v", sent[0])
	}
	for i, s := range sent {
		if s.Get("ClientToken") != token {
			t.Errorf("attempt %d token = %q, want %q", i+1, s.Get("ClientToken"), token)
		}
	}
}

// Two separate creates are two logical calls, so they carry two tokens.
func TestCreatesSendDifferentTokens(t *testing.T) {
	f := &fakeRouteTables{}
	client := f.serve(t)
	lossy := &lossyEC2{action: "CreateRouteTable"}
	lossy.front(t, client)
	for range 2 {
		if _, err := client.Create(context.Background(), routeTable, map[string]any{"VpcId": "vpc-1", "Tags": []any{routeTableNameTag}}); err != nil {
			t.Fatal(err)
		}
	}
	sent := lossy.attempts()
	if len(sent) != 2 || sent[0].Get("ClientToken") == "" || sent[0].Get("ClientToken") == sent[1].Get("ClientToken") {
		t.Fatalf("tokens = %q, %q; want two different non-empty tokens", sent[0].Get("ClientToken"), sent[1].Get("ClientToken"))
	}
}

// A create with no token, no name and no idempotent flag may make a second
// instance when sent again, so a lost response fails it; a throttle, which
// the service did not act on, is retried.
func TestUnnamedCreateRetriesOnlyAThrottle(t *testing.T) {
	desired := map[string]any{"CidrBlock": "10.99.0.0/16", "Tags": []any{vpcNameTag}}
	dropped := &fakeVPC{}
	client := dropped.serve(t)
	lossy := &lossyEC2{action: "CreateVpc", mode: "drop", fails: 1}
	lossy.front(t, client)
	if _, err := client.Create(context.Background(), vpcType, desired); err == nil {
		t.Fatal("a create whose response was lost succeeded")
	}
	if n := len(lossy.attempts()); n != 1 {
		t.Fatalf("dropped create attempts = %d, want 1", n)
	}

	throttled := &fakeVPC{}
	client = throttled.serve(t)
	lossy = &lossyEC2{action: "CreateVpc", mode: "throttle", fails: 1}
	lossy.front(t, client)
	if _, err := client.Create(context.Background(), vpcType, desired); err != nil {
		t.Fatalf("a throttled create: %v", err)
	}
	if n := len(lossy.attempts()); n != 2 {
		t.Fatalf("throttled create attempts = %d, want 2", n)
	}
}

// A create the override declares idempotent takes the retries a read does.
func TestIdempotentCreateRetriesADroppedConnection(t *testing.T) {
	desired := map[string]any{"DelaySeconds": 5, "Tags": []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-q"}}}
	f := &flakyQueue{answer: "drop", fails: map[string]int{"AmazonSQS.CreateQueue": 1}}
	if _, err := f.serve(t).Create(context.Background(), "AWS::SQS::Queue", desired); err != nil {
		t.Fatalf("Create = %v, want the retry to succeed", err)
	}
	if n := f.count("AmazonSQS.CreateQueue"); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
}

// The token member comes from the model's trait, on every create that has
// one, and only the members the templates do not set.
func TestCompiledCreatesCarryTheirToken(t *testing.T) {
	if got := readers[routeTable].Create.TokenMember; got != "ClientToken" {
		t.Fatalf("CreateRouteTable TokenMember = %q, want ClientToken", got)
	}
	if _, ok := readers[routeTable].Create.Form["ClientToken"]; !ok {
		t.Fatal("CreateRouteTable has no form step for ClientToken")
	}
	for name, r := range readers {
		if r.Create != nil && r.Create.TokenMember != "" && name != routeTable {
			t.Errorf("%s creates with token %s; the model has one on CreateRouteTable only", name, r.Create.TokenMember)
		}
	}
}

// An override may declare a create idempotent only when it is named.
func TestCompileRefusesIdempotentUnnamedCreate(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		if o.Type != "AWS::SQS::Queue" {
			continue
		}
		c := *o.Create
		c.Name = nil
		c.Idempotent = true
		o.Create = &c
		if _, errs := compileOne(files, lock, o); !containsErr(errs, "create is idempotent, but neither names") {
			t.Fatalf("errors = %v", errs)
		}
	}
}

// The token of a JSON protocol call is a body member.
func TestTokenBindingsForAJSONProtocol(t *testing.T) {
	b, err := tokenBindings("awsJson1_1", MutationCall{TokenMember: "ClientRequestToken"})
	if err != nil || len(b) != 1 || b[0].Member != "ClientRequestToken" || b[0].Location != "body" {
		t.Fatalf("bindings = %v, %v", b, err)
	}
	if s, _ := b[0].Structured.(string); len(s) != 36 {
		t.Fatalf("token = %v, want a UUID", b[0].Structured)
	}
}
