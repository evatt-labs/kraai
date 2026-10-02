package direct

import (
	"context"
	"encoding/json"
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

const (
	smType = "AWS::SecretsManager::Secret"
	smARN  = "arn:aws:secretsmanager:us-east-1:1:secret:kraai-e-secret-AbCdEf"
	// secretValue stands for a real secret: it must appear in no read, no
	// error and no call but the ones that write it.
	secretValue = "kraai-test-sentinel-not-a-secret"
)

// fakeSecrets is one secret as Secrets Manager holds it. Its value is held
// where GetSecretValue would answer it, and DescribeSecret answers with the
// value too, which the real service never does: a reader that mapped it
// would show.
type fakeSecrets struct {
	mu      sync.Mutex
	exists  bool
	secret  map[string]any
	tags    map[string]string
	regions map[string]string
	value   string
	// failCreate answers a create with this error code.
	failCreate string
	calls      map[string][]map[string]any
}

func (f *fakeSecrets) serve(t *testing.T) *Client {
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
		answer := func(v any) {
			body, _ := json.Marshal(v)
			_, _ = w.Write(body)
		}
		fail := func(code string) {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"__type":"`+code+`","message":"refused"}`)
		}
		switch op {
		case "CreateSecret":
			if f.failCreate != "" {
				fail(f.failCreate)
				return
			}
			f.exists = true
			f.secret = map[string]any{"ARN": smARN, "Name": in["Name"]}
			for _, k := range []string{"Description", "KmsKeyId"} {
				if v, ok := in[k]; ok {
					f.secret[k] = v
				}
			}
			f.tags, f.regions = map[string]string{}, map[string]string{}
			for _, tag := range asList(in["Tags"]) {
				m := tag.(map[string]any)
				f.tags[m["Key"].(string)] = m["Value"].(string)
			}
			for _, region := range asList(in["AddReplicaRegions"]) {
				m := region.(map[string]any)
				f.regions[m["Region"].(string)], _ = m["KmsKeyId"].(string)
			}
			f.value, _ = in["SecretString"].(string)
			answer(map[string]any{"ARN": smARN, "Name": in["Name"], "VersionId": "v1"})
		case "UpdateSecret":
			for _, k := range []string{"Description", "KmsKeyId"} {
				if v, ok := in[k]; ok {
					f.secret[k] = v
				}
			}
			if v, ok := in["SecretString"].(string); ok {
				f.value = v
			}
			answer(map[string]any{"ARN": smARN})
		case "TagResource":
			for _, tag := range asList(in["Tags"]) {
				m := tag.(map[string]any)
				f.tags[m["Key"].(string)] = m["Value"].(string)
			}
			answer(map[string]any{})
		case "UntagResource":
			for _, k := range asList(in["TagKeys"]) {
				delete(f.tags, k.(string))
			}
			answer(map[string]any{})
		case "ReplicateSecretToRegions":
			for _, region := range asList(in["AddReplicaRegions"]) {
				m := region.(map[string]any)
				f.regions[m["Region"].(string)], _ = m["KmsKeyId"].(string)
			}
			answer(map[string]any{"ARN": smARN})
		case "RemoveRegionsFromReplication":
			for _, region := range asList(in["RemoveReplicaRegions"]) {
				delete(f.regions, region.(string))
			}
			answer(map[string]any{"ARN": smARN})
		case "DeleteSecret":
			if len(f.regions) > 0 {
				fail("InvalidParameterException")
				return
			}
			f.exists = false
			answer(map[string]any{"ARN": smARN})
		case "DescribeSecret":
			if !f.exists {
				fail("ResourceNotFoundException")
				return
			}
			out := map[string]any{"VersionIdsToStages": map[string]any{"v1": []any{"AWSCURRENT"}}, "SecretString": secretValue}
			for k, v := range f.secret {
				out[k] = v
			}
			tags := []any{}
			for k, v := range f.tags {
				tags = append(tags, map[string]any{"Key": k, "Value": v})
			}
			out["Tags"] = tags
			status := []any{}
			for region, key := range f.regions {
				s := map[string]any{"Region": region, "Status": "InSync"}
				if key != "" {
					s["KmsKeyId"] = key
				}
				status = append(status, s)
			}
			if len(status) > 0 {
				out["ReplicationStatus"] = status
			}
			answer(out)
		case "GetSecretValue":
			answer(map[string]any{"ARN": smARN, "SecretString": f.value})
		default:
			fail("UnknownOperationException")
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

var secretNameTag = map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-secret"}

// targets is every operation a reader or any of its mutations calls.
func targets(r Reader) []string {
	out := []string{r.Target}
	for _, a := range r.Also {
		out = append(out, targets(a)...)
	}
	add := func(m *MutationCall) {
		if m != nil {
			out = append(out, m.Target)
		}
	}
	add(r.Create)
	add(r.Delete)
	for _, u := range r.Update {
		add(&u)
		add(u.Add)
		add(u.Remove)
		add(u.Change)
	}
	if r.List != nil {
		out = append(out, r.List.Target)
	}
	if r.Probe != nil {
		out = append(out, r.Probe.Target)
	}
	return out
}

// No call of the type reads a secret's value: the schema marks it
// write-only, so a read has no use for it, and holding it only widens what a
// log, a trace or an error could leak.
func TestSecretNoCallReadsTheSecretValue(t *testing.T) {
	for _, target := range targets(readers[smType]) {
		if strings.Contains(target, "GetSecretValue") || strings.Contains(target, "BatchGetSecretValue") {
			t.Errorf("the type calls %s", target)
		}
	}
	if readers[smType].Target != "secretsmanager.DescribeSecret" {
		t.Errorf("read target = %s, want DescribeSecret", readers[smType].Target)
	}
}

// Run end to end, the value goes out with the create and the update that
// set it, comes back in no read, and GetSecretValue is never called.
func TestSecretLifecycleNeverReadsTheValue(t *testing.T) {
	f := &fakeSecrets{}
	client := f.serve(t)
	ctx := context.Background()
	id, err := client.Create(ctx, smType, map[string]any{"SecretString": secretValue, "Tags": []any{secretNameTag}})
	if err != nil {
		t.Fatal(err)
	}
	if id != smARN {
		t.Fatalf("id = %q, want the ARN the create answered", id)
	}
	reads := func() string {
		props, err := client.ReadByID(ctx, smType, id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(props)
		return string(raw)
	}
	if got := reads(); strings.Contains(got, secretValue) || strings.Contains(got, "SecretString") {
		t.Fatalf("a read carries the secret: %s", got)
	}
	current, _ := client.ReadByID(ctx, smType, id)
	if err := client.Update(ctx, smType, id, current, map[string]any{"SecretString": secretValue + "-2", "Description": "d"}); err != nil {
		t.Fatal(err)
	}
	if got := reads(); strings.Contains(got, secretValue) {
		t.Fatalf("a read after the update carries the secret: %s", got)
	}
	if err := client.Delete(ctx, smType, id); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["GetSecretValue"]); n != 0 {
		t.Fatalf("GetSecretValue called %d times, want 0", n)
	}
	if f.value != secretValue+"-2" {
		t.Errorf("the update did not set the value")
	}
}

// The create names the secret by the identity tag, sends the value, tags and
// replicas, and answers the ARN the service made.
func TestCreateSecret(t *testing.T) {
	f := &fakeSecrets{}
	client := f.serve(t)
	_, err := client.Create(context.Background(), smType, map[string]any{
		"Description":    "d",
		"SecretString":   secretValue,
		"Tags":           []any{secretNameTag},
		"ReplicaRegions": []any{map[string]any{"Region": "us-west-2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.calls["CreateSecret"]
	if len(calls) != 1 {
		t.Fatalf("CreateSecret calls = %v", calls)
	}
	got := calls[0]
	token, _ := got["ClientRequestToken"].(string)
	delete(got, "ClientRequestToken")
	want := map[string]any{
		"Name": "kraai-e-secret", "Description": "d", "SecretString": secretValue,
		"Tags":              []any{secretNameTag},
		"AddReplicaRegions": []any{map[string]any{"Region": "us-west-2"}},
	}
	if !reflect.DeepEqual(got, want) || token == "" {
		t.Fatalf("CreateSecret = %v (token %q), want %v and a token", got, token, want)
	}
}

// Cloud Control deletes a secret at once, so the direct delete forces it:
// a recovery window would leave the name held and the secret readable.
func TestDeleteSecretForcesDeletion(t *testing.T) {
	f := &fakeSecrets{}
	client := f.serve(t)
	ctx := context.Background()
	id, err := client.Create(ctx, smType, map[string]any{"Tags": []any{secretNameTag}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(ctx, smType, id); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"SecretId": smARN, "ForceDeleteWithoutRecovery": true}}
	if got := f.calls["DeleteSecret"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("DeleteSecret calls = %v, want %v", got, want)
	}
	// Already gone is deleted already.
	if err := client.Delete(ctx, smType, id); err != nil {
		t.Fatalf("Delete of a secret already gone = %v, want done", err)
	}
}

// A secret with replicas cannot be deleted until they are removed, as Cloud
// Control removes them first.
func TestDeleteSecretRemovesReplicasFirst(t *testing.T) {
	f := &fakeSecrets{}
	client := f.serve(t)
	ctx := context.Background()
	id, err := client.Create(ctx, smType, map[string]any{
		"Tags":           []any{secretNameTag},
		"ReplicaRegions": []any{map[string]any{"Region": "us-west-2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(ctx, smType, id); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"SecretId": smARN, "RemoveReplicaRegions": []any{"us-west-2"}}}
	if got := f.calls["RemoveRegionsFromReplication"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("RemoveRegionsFromReplication calls = %v, want %v", got, want)
	}
}

// Only the properties that changed are sent: tags alone make no UpdateSecret
// call, and so create no new version of the secret.
func TestUpdateSecretSendsOnlyWhatChanged(t *testing.T) {
	f := &fakeSecrets{}
	client := f.serve(t)
	ctx := context.Background()
	id, err := client.Create(ctx, smType, map[string]any{"SecretString": secretValue, "Tags": []any{secretNameTag}})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := client.ReadByID(ctx, smType, id)
	team := map[string]any{"Key": "team", "Value": "kraai"}
	if err := client.Update(ctx, smType, id, current, map[string]any{"Tags": []any{secretNameTag, team}}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["UpdateSecret"]); n != 0 {
		t.Fatalf("UpdateSecret called %d times for a tag change, want 0", n)
	}
	want := []map[string]any{{"SecretId": smARN, "Tags": []any{team}}}
	if got := f.calls["TagResource"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("TagResource calls = %v, want %v", got, want)
	}
}

// What no direct call sets goes through Cloud Control, decided before any
// call is made.
func TestSecretRoutesWhatNoCallSetsToCloudControl(t *testing.T) {
	if !CanMutateWith(smType, map[string]any{"SecretString": "x", "Description": "d"}) {
		t.Error("a plain secret is not mutable directly")
	}
	for _, p := range []string{"GenerateSecretString", "Type"} {
		if CanMutateWith(smType, map[string]any{p: "x"}) {
			t.Errorf("a secret naming %s is mutable directly, want Cloud Control", p)
		}
	}
}

// A refused create's error names the operation and the service's reason,
// never the value that was sent.
func TestSecretErrorsDoNotCarryTheValue(t *testing.T) {
	f := &fakeSecrets{failCreate: "InvalidRequestException"}
	client := f.serve(t)
	_, err := client.Create(context.Background(), smType, map[string]any{"SecretString": secretValue, "Tags": []any{secretNameTag}})
	if err == nil {
		t.Fatal("Create = nil, want the refusal")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("the error carries the secret: %v", err)
	}
}

// The planner never finds the write-only value changed, but it adds the
// value to the patch of any other update, as it does for Cloud Control, so
// one UpdateSecret carries both and sets a new version.
func TestUpdateSecretSendsTheValueBesideAChange(t *testing.T) {
	f := &fakeSecrets{}
	client := f.serve(t)
	ctx := context.Background()
	id, err := client.Create(ctx, smType, map[string]any{"SecretString": secretValue, "Tags": []any{secretNameTag}})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := client.ReadByID(ctx, smType, id)
	if err := client.Update(ctx, smType, id, current, map[string]any{"Description": "d", "SecretString": secretValue}); err != nil {
		t.Fatal(err)
	}
	calls := f.calls["UpdateSecret"]
	if len(calls) != 1 {
		t.Fatalf("UpdateSecret calls = %d, want 1", len(calls))
	}
	delete(calls[0], "ClientRequestToken")
	want := map[string]any{"SecretId": smARN, "Description": "d", "SecretString": secretValue}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("UpdateSecret = %v, want %v", calls[0], want)
	}
}
