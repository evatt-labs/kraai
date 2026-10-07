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
			f := m["models/ssm-2014-11-06.json"]
			f.Data = append(append([]byte{}, f.Data...), ' ')
		}, "regenerate"},
		"an edited schema": {func(m fstest.MapFS) {
			f := m["schemas/AWS--SSM--Parameter.json"]
			f.Data = []byte(strings.Replace(string(f.Data), "Parameter", "Parametre", 1))
		}, "regenerate"},
		"an operation the model lacks": {func(m fstest.MapFS) {
			f := m["overrides/AWS--SSM--Parameter.yaml"]
			f.Data = []byte(strings.Replace(string(f.Data), "GetParameter", "GetParamete", 1))
		}, "defines no operation GetParamete"},
		"an unlocked model": {func(m fstest.MapFS) {
			f := m["overrides/AWS--SSM--Parameter.yaml"]
			f.Data = []byte(strings.ReplaceAll(string(f.Data), "2014-11-06", "2099-01-01"))
		}, "which the lock does not record"},
		"an unknown key": {func(m fstest.MapFS) {
			f := m["overrides/AWS--SSM--Parameter.yaml"]
			f.Data = append(append([]byte{}, f.Data...), []byte("operaton: GetParameter\n")...)
		}, "not found"},
		"a misnamed file": {func(m fstest.MapFS) {
			m["overrides/AWS--SSM--Parameters.yaml"] = m["overrides/AWS--SSM--Parameter.yaml"]
			delete(m, "overrides/AWS--SSM--Parameter.yaml")
		}, "must be named AWS--SSM--Parameter.yaml"},
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
	text := string(f.Data)
	if strings.Contains(text, "\nskip:\n") {
		text = strings.Replace(text, "\nskip:\n", "\nskip:\n  "+property+": "+reason+"\n", 1)
	} else {
		text += "skip:\n  " + property + ": " + reason + "\n"
	}
	m["overrides/"+file] = &fstest.MapFile{Data: []byte(text)}
	return m
}
