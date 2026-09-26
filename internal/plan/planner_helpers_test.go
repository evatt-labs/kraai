package plan

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

const envName = "swift-otter-badger-10203"

func findAction(t *testing.T, p *Plan, provider, typ string) Action {
	t.Helper()
	for _, a := range p.Actions {
		if a.Provider == provider && a.Type == typ {
			return a
		}
	}
	t.Fatalf("no action for %s/%s in %+v", provider, typ, p.Actions)
	return Action{}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
func assertValidationError(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want a validation error")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
		t.Fatalf("err = %v, want a kerrors.CodeValidation error", err)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("err = %q, want it to mention %q", err.Error(), wantSubstr)
	}
}
func bindingName(i int) string {
	return fmt.Sprintf("BINDING_%d", i)
}
