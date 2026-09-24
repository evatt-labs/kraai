package direct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const routers = "AWS::Bedrock::IntelligentPromptRouter"

// pages answers each list request by the page token it carries, recording
// every request's query and path.
func pages(t *testing.T, respond func(token string) (int, string)) (*Client, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		status, body := respond(r.URL.Query().Get("nextToken"))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP:        srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region:      "us-east-1",
		Endpoint:    func(string, string) string { return srv.URL },
		Now:         func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}, &seen
}

// Every page is read, in order, each request carrying the fixed filter and
// the previous page's token.
func TestListReadsEveryPage(t *testing.T) {
	client, seen := pages(t, func(token string) (int, string) {
		switch token {
		case "":
			return 200, `{"promptRouterSummaries":[{"promptRouterArn":"a"},{"promptRouterArn":"b"}],"nextToken":"t1"}`
		case "t1":
			return 200, `{"promptRouterSummaries":[{"promptRouterArn":"c"}]}`
		}
		return 500, `{}`
	})
	ids, err := client.List(context.Background(), routers)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Fatalf("List = %v", ids)
	}
	want := []string{"GET /prompt-routers?type=custom", "GET /prompt-routers?nextToken=t1&type=custom"}
	if !reflect.DeepEqual(*seen, want) {
		t.Fatalf("requests = %v, want %v", *seen, want)
	}
}

func TestListWithNoItemsIsEmpty(t *testing.T) {
	client, _ := pages(t, func(string) (int, string) { return 200, `{}` })
	if ids, err := client.List(context.Background(), routers); err != nil || len(ids) != 0 {
		t.Fatalf("List = %v, %v", ids, err)
	}
}

// A list that cannot be vouched complete is an error, never a shorter list:
// an instance missed would be taken for absent and created again.
func TestListFailsClosed(t *testing.T) {
	cases := map[string]struct {
		respond func(string) (int, string)
		want    string
	}{
		"an item without its identifier": {func(string) (int, string) {
			return 200, `{"promptRouterSummaries":[{"promptRouterArn":"a"},{"promptRouterName":"b"}]}`
		}, "without its promptRouterArn"},
		"items that are not a list": {func(string) (int, string) {
			return 200, `{"promptRouterSummaries":{"promptRouterArn":"a"}}`
		}, "not as a list"},
		"a repeated page token": {func(string) (int, string) {
			return 200, `{"promptRouterSummaries":[],"nextToken":"same"}`
		}, `page token "same" twice`},
		"more pages than the cap": {func(token string) (int, string) {
			n := 0
			_, _ = fmt.Sscanf(token, "t%d", &n)
			return 200, fmt.Sprintf(`{"promptRouterSummaries":[],"nextToken":"t%d"}`, n+1)
		}, "ran past 1000 pages"},
		"a service error": {func(string) (int, string) {
			return 403, `{"message":"no"}`
		}, "HTTP 403"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := pages(t, c.respond)
			ids, err := client.List(context.Background(), routers)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("List = %v, %v; want an error containing %q", ids, err, c.want)
			}
			var apiErr *APIError
			if name == "a service error" && !errors.As(err, &apiErr) {
				t.Fatalf("the service error was not an *APIError: %v", err)
			}
		})
	}
}

func TestHasList(t *testing.T) {
	if !HasList(routers) || HasList("AWS::XRay::Group") || HasList("AWS::Nope::Thing") {
		t.Fatal("HasList disagrees with the overrides")
	}
	client, _ := pages(t, func(string) (int, string) { return 200, `{}` })
	if _, err := client.List(context.Background(), "AWS::XRay::Group"); err == nil {
		t.Fatal("a type with no list was listed")
	}
}

func TestCompileRefusesAList(t *testing.T) {
	const bedrock = "AWS--Bedrock--IntelligentPromptRouter.yaml"
	cases := map[string]struct {
		old, replacement, want string
	}{
		"an operation the model lacks":          {"operation: ListPromptRouters", "operation: ListRouters", "list operation ListRouters is not in the model"},
		"an operation that is not paginated":    {"operation: ListPromptRouters", "operation: GetPromptRouter", "is not paginated"},
		"a value the enum lacks":                {"type: custom", "type: mine", `list input type is "mine", which is not one of [custom default]`},
		"an input the operation lacks":          {"type: custom", "kind: custom", "list input kind is not a member"},
		"an item member the items lack":         {"item: promptRouterArn", "item: routerArn", "list item member routerArn is not in the items"},
		"an item other than the read's binding": {"item: promptRouterArn", "item: promptRouterName", "is not promptRouterArn, the member the read binds PromptRouterArn to"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(edit(t, bedrock, c.old, c.replacement))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant an error containing %q", err, c.want)
			}
		})
	}
}
