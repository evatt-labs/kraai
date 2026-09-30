package direct

import "testing"

func TestCompileUnsupported(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	var base Override
	for _, o := range all {
		if o.Type == sqsQueue {
			base = o
		}
	}
	// Without the tags call Tags has no route; declared unsupported it
	// counts as one.
	noTags := base
	noTags.Update = base.Update[:1]
	r, errs := compileOne(files, lock, noTags)
	if len(errs) > 0 || r.LifecycleComplete {
		t.Fatalf("errors %v, complete %v; want complete false with Tags unrouted", errs, r.LifecycleComplete)
	}
	noTags.Unsupported = map[string]string{"Tags": "tagged by a call nothing writes"}
	r, errs = compileOne(files, lock, noTags)
	if len(errs) > 0 || !r.LifecycleComplete || r.Unsupported["Tags"] == "" {
		t.Fatalf("errors %v, complete %v, unsupported %v; want complete with Tags unsupported", errs, r.LifecycleComplete, r.Unsupported)
	}

	for name, c := range map[string]struct {
		unsupported map[string]string
		want        string
	}{
		"a property the schema lacks": {map[string]string{"Nope": "x"}, "unsupported Nope is not a property"},
		"a nested path the schema lacks": {map[string]string{"Tags.Nope": "x"},
			"unsupported Tags.Nope is not a property"},
		"a property that is also routed": {map[string]string{"DelaySeconds": "x"}, "unsupported DelaySeconds is also routed"},
		"no reason":                      {map[string]string{"Tags": ""}, "unsupported Tags gives no reason"},
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			o.Unsupported = c.unsupported
			if _, errs := compileOne(files, lock, o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}
