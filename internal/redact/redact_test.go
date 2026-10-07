package redact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

func TestStringReplacesEverySensitiveValue(t *testing.T) {
	s := &Set{}
	s.Add("hunter2", "terraform.base.password")
	s.Add("hunter2-long", "terraform.base.token")
	s.Add("abc", "terraform.base.short")
	got := s.String("pw hunter2, token hunter2-long, short abc")
	want := "pw [sensitive terraform.base.password], token [sensitive terraform.base.token], short abc"
	if got != want {
		t.Fatalf("String = %q\nwant %q", got, want)
	}
}

func TestANilOrEmptySetRedactsNothing(t *testing.T) {
	var nilSet *Set
	nilSet.Add("hunter2", "x")
	if got := nilSet.String("hunter2"); got != "hunter2" {
		t.Fatalf("nil String = %q", got)
	}
	if got := (&Set{}).String("hunter2"); got != "hunter2" {
		t.Fatalf("empty String = %q", got)
	}
	if From(context.Background()) != nil {
		t.Fatal("a context carrying no set has one")
	}
}

// A redacted error keeps the original's identity, so its exit code and
// errors.Is still work, and every rendering of it is redacted, including
// the stack trace --debug prints.
func TestErrorRedactsEveryRendering(t *testing.T) {
	s := &Set{}
	s.Add("hunter2", "terraform.base.password")
	cause := kerrors.Validation("version: cannot unmarshal hunter2")
	err := s.Error(cause)
	for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%s", err)} {
		if strings.Contains(rendered, "hunter2") || !strings.Contains(rendered, "[sensitive terraform.base.password]") {
			t.Errorf("rendered %q", rendered)
		}
	}
	if !errors.Is(err, cause) {
		t.Error("the redacted error lost its cause")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
		t.Error("the redacted error lost its code")
	}
	if s.Error(nil) != nil {
		t.Error("a nil error came back non-nil")
	}
}

func TestWriterRedacts(t *testing.T) {
	s := &Set{}
	var out bytes.Buffer
	w := s.Writer(&out)
	// Added after the writer is made, as a manifest is read after the
	// command's output is set up.
	s.Add("hunter2", "terraform.base.password")
	if n, err := fmt.Fprintf(w, "value: hunter2\n"); err != nil || n != len("value: hunter2\n") {
		t.Fatalf("Fprintf = %d, %v", n, err)
	}
	if out.String() != "value: [sensitive terraform.base.password]\n" {
		t.Fatalf("wrote %q", out.String())
	}
}

func TestWithCarriesTheSet(t *testing.T) {
	s := &Set{}
	if From(With(context.Background(), s)) != s {
		t.Fatal("the set was not carried")
	}
}
