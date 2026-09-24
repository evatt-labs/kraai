package direct

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// copyFS copies the embedded files into a map a test can change.
func copyFS(t *testing.T) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	err := fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := files.ReadFile(name)
		out[name] = &fstest.MapFile{Data: raw}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheCheckedInSubsetVerifies(t *testing.T) {
	if err := Verify(); err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil || len(all) == 0 {
		t.Fatalf("Overrides = %d, %v", len(all), err)
	}
}

func TestVerifyCatches(t *testing.T) {
	cases := map[string]struct {
		change func(fstest.MapFS)
		want   string
	}{
		"an edited model": {func(m fstest.MapFS) {
			f := m["models/xray-2016-04-12.json"]
			f.Data = append(append([]byte{}, f.Data...), ' ')
		}, "regenerate"},
		"an edited schema": {func(m fstest.MapFS) {
			f := m["schemas/AWS--XRay--Group.json"]
			f.Data = []byte(strings.Replace(string(f.Data), "GroupName", "GroupNom", 1))
		}, "regenerate"},
		"an operation the model lacks": {func(m fstest.MapFS) {
			f := m["overrides/AWS--XRay--Group.yaml"]
			f.Data = []byte(strings.Replace(string(f.Data), "GetGroup", "GetGroups", 1))
		}, "defines no operation GetGroups"},
		"an unlocked model": {func(m fstest.MapFS) {
			f := m["overrides/AWS--XRay--Group.yaml"]
			f.Data = []byte(strings.ReplaceAll(string(f.Data), "2016-04-12", "2099-01-01"))
		}, "which the lock does not record"},
		"an unknown key": {func(m fstest.MapFS) {
			f := m["overrides/AWS--XRay--Group.yaml"]
			f.Data = append(append([]byte{}, f.Data...), []byte("operaton: GetGroup\n")...)
		}, "not found"},
		"a misnamed file": {func(m fstest.MapFS) {
			m["overrides/AWS--XRay--Groups.yaml"] = m["overrides/AWS--XRay--Group.yaml"]
			delete(m, "overrides/AWS--XRay--Group.yaml")
		}, "must be named AWS--XRay--Group.yaml"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := copyFS(t)
			c.change(m)
			if err := verify(m); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("verify = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// withSkip adds a skip entry to one override file.
func withSkip(m fstest.MapFS, file, property, reason string) fstest.MapFS {
	f := m["overrides/"+file]
	m["overrides/"+file] = &fstest.MapFile{Data: []byte(strings.Replace(string(f.Data), "skip:\n", "skip:\n  "+property+": "+reason+"\n", 1))}
	return m
}
