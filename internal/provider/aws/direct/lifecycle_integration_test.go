//go:build integration

package direct

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

var updateLifecycle = flag.Bool("update-lifecycle", false, "merge this run into evidence/lifecycle.json")

// Lifecycle parity, which creates and deletes real instances: each type
// with a lifecycle vector is created through its direct mutations, updated
// one property at a time and deleted, and after each step read back through
// Cloud Control. Runs only with KRAAI_ALLOW_MUTATE=1; every instance is
// named kraai-lifecycle-* and deleted when the test ends, however it ends.
//
// A vector may name a resource made for the harness, such as a key that
// bills while it exists or a subnet a type needs: {kmsKeyArn} is read from
// KRAAI_LIFECYCLE_KMS_KEY_ARN, {subnetIdA} from KRAAI_LIFECYCLE_SUBNET_ID_A.
// An update naming one that is unset is skipped, and a type whose create
// names one is skipped and records nothing.
//
//	KRAAI_ALLOW_MUTATE=1 go test -tags integration ./internal/provider/aws/direct -run TestLifecycleParity [-args -update-lifecycle]
func TestLifecycleParity(t *testing.T) {
	if os.Getenv("KRAAI_ALLOW_MUTATE") != "1" {
		t.Skip("creates and deletes real resources; set KRAAI_ALLOW_MUTATE=1")
	}
	const region = "us-east-1"
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	cc := cloudcontrol.NewFromConfig(cfg)
	who, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}
	account = aws.ToString(who.Account)
	client := &Client{HTTP: &http.Client{Timeout: 30 * time.Second}, Credentials: cfg.Credentials, Region: region}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	var run LifecycleEvidence
	for _, o := range all {
		if o.Lifecycle == nil {
			continue
		}
		hash, err := OverrideHash(o.Type)
		if err != nil {
			t.Fatal(err)
		}
		e := TypeLifecycle{Type: o.Type, Date: time.Now().UTC().Format("2006-01-02"), Override: hash, Outcome: "parity"}
		ran, skipped := false, false
		passed := t.Run(o.Type, func(t *testing.T) {
			ran = true
			defer func() { skipped = t.Skipped() }()
			e = lifecycle(ctx, t, cc, client, o, e)
		})
		// A type -run leaves out, or that could not run, is not evidence
		// either way.
		if !ran || skipped {
			continue
		}
		// A step that stops the subtest, such as a failed create, never
		// returns its outcome.
		if !passed {
			e.Outcome = "differs"
		}
		t.Logf("%s: lifecycle %s, updated %v", o.Type, e.Outcome, e.Updated)
		run.Types = append(run.Types, e)
	}
	if !*updateLifecycle {
		return
	}
	var prior LifecycleEvidence
	if raw, err := os.ReadFile("evidence/lifecycle.json"); err == nil {
		if err := json.Unmarshal(raw, &prior); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.MarshalIndent(MergeLifecycle(prior, run), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("evidence/lifecycle.json", append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func lifecycle(ctx context.Context, t *testing.T, cc *cloudcontrol.Client, client *Client, o Override, e TypeLifecycle) TypeLifecycle {
	name := fmt.Sprintf("kraai-lifecycle-%s-%d", strings.ToLower(o.Type[strings.LastIndex(o.Type, ":")+1:]), time.Now().Unix())
	nameTag := map[string]any{"Key": "kraai:resource-name", "Value": name}
	// A vector may name the instance, {name}, where, {account} and
	// {region}, and resources made for the harness, from the environment.
	pairs := []string{"{name}", name, "{account}", account, "{region}", client.Region}
	unset := map[string]bool{}
	for _, v := range vectorVars(o.Lifecycle) {
		if value := os.Getenv(envName(v)); value != "" {
			pairs = append(pairs, "{"+v+"}", value)
		} else {
			unset[v] = true
		}
	}
	vars := strings.NewReplacer(pairs...)
	if missing := namesUnset(o.Lifecycle.Create, unset); missing != "" {
		t.Skipf("the create needs %s, which is unset", missing)
	}
	desired := fill(o.Lifecycle.Create, vars).(map[string]any)
	desired["Tags"] = append([]any{nameTag}, asList(desired["Tags"])...)

	id, err := client.Create(ctx, o.Type, desired)
	deleted := false
	// A create that fails after the instance exists still returns its
	// identifier, and the instance is deleted.
	if id != "" {
		t.Cleanup(func() {
			if !deleted {
				if err := client.Delete(context.Background(), o.Type, id); err != nil {
					t.Errorf("cleanup delete of %s: %v", id, err)
				}
			}
		})
	}
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fail := func(step string, err error) {
		t.Errorf("%s: %v", step, err)
		e.Outcome = "differs"
	}
	if err := ccShows(ctx, cc, o.Type, id, desired); err != nil {
		fail("create read through Cloud Control", err)
	}
	// Every update must keep what came before it, not only set its own
	// property: a call that replaces the whole instance could reset the
	// rest.
	shown := maps.Clone(desired)
	for _, property := range sortedKeys(o.Lifecycle.Update) {
		if missing := namesUnset(o.Lifecycle.Update[property], unset); missing != "" {
			t.Logf("%s: skipped, %s is unset", property, missing)
			continue
		}
		value := fill(o.Lifecycle.Update[property], vars)
		if property == "Tags" {
			value = append([]any{nameTag}, asList(value)...)
		}
		current, err := client.ReadByID(ctx, o.Type, id)
		if err != nil {
			fail("read before updating "+property, err)
			continue
		}
		changes := map[string]any{property: value}
		if err := client.Update(ctx, o.Type, id, current, changes); err != nil {
			fail("update "+property, err)
			continue
		}
		shown[property] = value
		if err := ccShows(ctx, cc, o.Type, id, shown); err != nil {
			fail("update "+property+" read through Cloud Control", err)
			continue
		}
		e.Updated = append(e.Updated, property)
	}
	if err := client.Delete(ctx, o.Type, id); err != nil {
		fail("delete", err)
		return e
	}
	deleted = true
	if err := ccAbsent(ctx, cc, o.Type, id); err != nil {
		fail("delete read through Cloud Control", err)
	}
	sort.Strings(e.Updated)
	return e
}

// ccShows polls Cloud Control's read of id until it covers want.
func ccShows(ctx context.Context, cc *cloudcontrol.Client, typeName, id string, want map[string]any) error {
	var last map[string]any
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		out, err := cc.GetResource(ctx, &cloudcontrol.GetResourceInput{TypeName: aws.String(typeName), Identifier: aws.String(id)})
		if err != nil {
			continue
		}
		last = nil
		if json.Unmarshal([]byte(aws.ToString(out.ResourceDescription.Properties)), &last) == nil && covers(want, last) {
			return nil
		}
	}
	return fmt.Errorf("Cloud Control's read never showed %v; last %v", want, last)
}

// ccAbsent polls Cloud Control until it reads id as absent.
func ccAbsent(ctx context.Context, cc *cloudcontrol.Client, typeName, id string) error {
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		_, err := cc.GetResource(ctx, &cloudcontrol.GetResourceInput{TypeName: aws.String(typeName), Identifier: aws.String(id)})
		var notFound *cctypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil
		}
	}
	return errors.New("Cloud Control still reads it")
}

// fill replaces the variables in every string of v, in a copy.
func fill(v any, vars *strings.Replacer) any {
	switch t := v.(type) {
	case string:
		return vars.Replace(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = fill(item, vars)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = fill(item, vars)
		}
		return out
	}
	return v
}

// vectorPlaceholder is a {camelName} in a vector.
var vectorPlaceholder = regexp.MustCompile(`\{([a-z][A-Za-z0-9]*)\}`)

// vectorVars lists the variables a vector names, beyond those the harness
// fills itself.
func vectorVars(l *Lifecycle) []string {
	seen := map[string]bool{"name": true, "account": true, "region": true}
	var out []string
	for _, m := range vectorPlaceholder.FindAllStringSubmatch(fmt.Sprint(l.Create, l.Update), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// envName is the environment variable a vector variable is read from:
// subnetIdA is KRAAI_LIFECYCLE_SUBNET_ID_A.
func envName(v string) string {
	var b strings.Builder
	for i, r := range v {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return "KRAAI_LIFECYCLE_" + b.String()
}

// namesUnset is the first unset variable v names, or "".
func namesUnset(v any, unset map[string]bool) string {
	for _, m := range vectorPlaceholder.FindAllStringSubmatch(fmt.Sprint(v), -1) {
		if unset[m[1]] {
			return envName(m[1])
		}
	}
	return ""
}
