package direct

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// tagListClient is a client over f with the parameter override's tags route
// replaced by a list route over the same map property, chunked as given,
// installed for the test: a map is routed as {Key, Value} elements.
func tagListClient(t *testing.T, f *fakeParameter, chunk int) *Client {
	t.Helper()
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(o Override) bool { return o.Type == parameterType })
	if i < 0 {
		t.Fatalf("no override for %s", parameterType)
	}
	o := all[i]
	o.Update = slices.Clone(o.Update)
	j := slices.IndexFunc(o.Update, func(u UpdateCall) bool { return u.Tags != nil })
	if j < 0 {
		t.Fatal("the parameter override has no tags route to replace")
	}
	address := func(k string, v any) map[string]any {
		return map[string]any{"ResourceType": "Parameter", "ResourceId": "{Name}", k: v}
	}
	o.Update[j] = UpdateCall{List: &ListRoute{
		Property: "Tags", Key: []string{"Key"}, Chunk: chunk,
		Element: map[string]any{"Key": "{Key}", "Value": "{Value}"},
		Add:     Mutation{Operation: "AddTagsToResource", Input: address("Tags", "{added}")},
		Remove:  &Mutation{Operation: "RemoveTagsFromResource", Input: address("TagKeys", "{removedKeys}")},
	}}
	r, errs := compileOne(files, lock, o)
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	old := readers[parameterType]
	readers[parameterType] = r
	t.Cleanup(func() { readers[parameterType] = old })
	client := f.serve(t)
	client.Wait = 300 * time.Millisecond
	return client
}

func tagMap(tags map[string]string) map[string]any {
	out := make(map[string]any, len(tags))
	for k, v := range tags {
		out[k] = v
	}
	return out
}

// A map property routed as elements is diffed by key, the entries added or
// changed and the keys removed each sent in calls of at most the chunk, and
// the update returns once the read holds exactly the map desired.
func TestListRouteOverAMapChunksAndWaitsForTheMap(t *testing.T) {
	f := existingParameter()
	f.tags = tagMap(map[string]string{"kraai:resource-name": "kraai-e-param", "x": "1", "y": "2", "z": "3"})
	client := tagListClient(t, f, 2)
	desired := tagMap(map[string]string{"kraai:resource-name": "kraai-e-param", "a": "1", "b": "2", "c": "3", "d": "4", "e": "5"})
	current := map[string]any{"Name": "kraai-e-param", "Tags": f.tags}
	if err := client.Update(context.Background(), parameterType, "kraai-e-param", current, map[string]any{"Tags": desired}); err != nil {
		t.Fatal(err)
	}
	var added, removed [][]string
	for _, c := range f.calls["AddTagsToResource"] {
		var keys []string
		for _, tag := range c["Tags"].([]any) {
			keys = append(keys, tag.(map[string]any)["Key"].(string))
		}
		added = append(added, keys)
	}
	for _, c := range f.calls["RemoveTagsFromResource"] {
		var keys []string
		for _, k := range c["TagKeys"].([]any) {
			keys = append(keys, k.(string))
		}
		removed = append(removed, keys)
	}
	if want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}; !reflect.DeepEqual(added, want) {
		t.Fatalf("added in calls %v, want %v", added, want)
	}
	if want := [][]string{{"x", "y"}, {"z"}}; !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed in calls %v, want %v", removed, want)
	}
	if !reflect.DeepEqual(f.tags, desired) {
		t.Fatalf("tags = %v, want %v", f.tags, desired)
	}
}

// A service can leave out an entry set to its default, so the wait does not
// expect the read to show one it never returns.
func TestListRouteOverAMapTakesAnOmittedEntryAsSet(t *testing.T) {
	f := existingParameter()
	f.tags = tagMap(map[string]string{"kraai:resource-name": "kraai-e-param"})
	f.hidden = "d"
	client := tagListClient(t, f, 20)
	desired := tagMap(map[string]string{"kraai:resource-name": "kraai-e-param", "d": "default"})
	current := map[string]any{"Name": "kraai-e-param", "Tags": tagMap(map[string]string{"kraai:resource-name": "kraai-e-param"})}
	if err := client.Update(context.Background(), parameterType, "kraai-e-param", current, map[string]any{"Tags": desired}); err != nil {
		t.Fatalf("Update = %v, want an entry the read omits taken as set", err)
	}
	if _, sent := f.tags["d"]; !sent {
		t.Fatalf("tags = %v, want the entry sent", f.tags)
	}
}

// Taking an omitted entry as set does not let a missed removal pass: an
// entry read that is no longer desired still fails the wait.
func TestListRouteOverAMapNoticesAMissedRemoval(t *testing.T) {
	f := existingParameter()
	f.tags = tagMap(map[string]string{"kraai:resource-name": "kraai-e-param", "old": "1"})
	f.stuck = "old"
	client := tagListClient(t, f, 20)
	current := map[string]any{"Name": "kraai-e-param", "Tags": f.tags}
	err := client.Update(context.Background(), parameterType, "kraai-e-param", current, map[string]any{"Tags": tagMap(map[string]string{"kraai:resource-name": "kraai-e-param"})})
	if err == nil || !strings.Contains(err.Error(), "was not visible after") {
		t.Fatalf("Update = %v, want the wait to give up on the entry still read", err)
	}
	if n := len(f.calls["RemoveTagsFromResource"]); n != 1 {
		t.Fatalf("RemoveTagsFromResource called %d times, want 1", n)
	}
}

// compileWhen compiles the parameter override with its further call made
// only when a read property has one of the values given.
func compileWhen(t *testing.T, when map[string][]string) (Reader, []error) {
	t.Helper()
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(o Override) bool { return o.Type == parameterType })
	if i < 0 {
		t.Fatalf("no override for %s", parameterType)
	}
	o := all[i]
	o.Also = slices.Clone(o.Also)
	o.Also[0].When = when
	return compileOne(files, lock, o)
}

// A further call is made only for an instance whose read property has one
// of the values given, for an operation the service refuses on others.
func TestAFurtherCallIsMadeOnlyWhen(t *testing.T) {
	r, errs := compileWhen(t, map[string][]string{"Type": {"SecureString"}})
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	old := readers[parameterType]
	readers[parameterType] = r
	t.Cleanup(func() { readers[parameterType] = old })
	for name, c := range map[string]struct {
		kind      string
		wantTags  bool
		wantCalls int
	}{
		"a value of another type": {"String", false, 0},
		"a value of the type":     {"SecureString", true, 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := existingParameter()
			f.param["Type"] = c.kind
			got, err := f.serve(t).Read(context.Background(), parameterType, map[string]string{"Name": "kraai-e-param"})
			if err != nil {
				t.Fatal(err)
			}
			if _, read := got["Tags"]; read != c.wantTags {
				t.Fatalf("Tags read = %v, want %v: %v", read, c.wantTags, got)
			}
			if n := len(f.calls["ListTagsForResource"]); n != c.wantCalls {
				t.Fatalf("ListTagsForResource called %d times, want %d", n, c.wantCalls)
			}
		})
	}
}

func TestCompileRefusesABadWhen(t *testing.T) {
	for name, c := range map[string]struct {
		when map[string][]string
		want string
	}{
		"a property the read lacks":         {map[string][]string{"Nope": {"x"}}, "is made when Nope, which is not a scalar property of the read"},
		"a property that is not a scalar":   {map[string][]string{"Tags": {"x"}}, "is made when Tags, which is not a scalar property of the read"},
		"a property with no value to match": {map[string][]string{"Type": nil}, "is made when Type has no value"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, errs := compileWhen(t, c.when); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}
