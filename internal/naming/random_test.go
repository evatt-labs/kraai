package naming

import (
	"errors"
	"testing"
	"testing/iotest"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

func TestRandomIntn_RejectsNonPositiveBound(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		_, err := randomIntn(n)
		if err == nil {
			t.Fatalf("randomIntn(%d): expected an error", n)
		}
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeUnexpected {
			t.Fatalf("randomIntn(%d) error is not a CodeUnexpected *kerrors.KError: %v", n, err)
		}
	}
}

// TestRandomIntn_ReaderErrorIsWrapped exercises the (in practice
// unreachable — crypto/rand.Reader never fails on a supported platform)
// read-error path via the randReader test seam, asserting it comes back
// as a CodeUnexpected *kerrors.KError rather than a bare error or panic,
// matching kraai's own typed-returned-error convention.
func TestRandomIntn_ReaderErrorIsWrapped(t *testing.T) {
	orig := randReader
	randReader = iotest.ErrReader(errors.New("injected read failure"))
	defer func() { randReader = orig }()

	_, err := randomIntn(5)
	if err == nil {
		t.Fatal("expected an error")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeUnexpected {
		t.Fatalf("error is not a CodeUnexpected *kerrors.KError: %v", err)
	}
}

func TestPick_PropagatesDrawError(t *testing.T) {
	orig := drawInt
	drawInt = func(int) (int64, error) { return 0, errors.New("boom") }
	defer func() { drawInt = orig }()

	_, err := pick(colors)
	if err == nil {
		t.Fatal("expected an error")
	}
}

// TestGenerateEnvironmentName_PropagatesEachDrawsError exercises every
// error-return branch in GenerateEnvironmentName (one per draw: color,
// adjective, animal, numeric suffix) by making exactly the Nth draw fail
// and the rest succeed, via the drawInt seam. crypto/rand's own Read call
// pattern isn't a stable enough seam to target "the third draw fails"
// directly — drawInt exists for exactly this.
func TestGenerateEnvironmentName_PropagatesEachDrawsError(t *testing.T) {
	orig := drawInt
	defer func() { drawInt = orig }()

	for failAt := 0; failAt < 4; failAt++ {
		calls := 0
		drawInt = func(n int) (int64, error) {
			defer func() { calls++ }()
			if calls == failAt {
				return 0, errors.New("boom")
			}
			return orig(n)
		}

		if _, err := GenerateEnvironmentName(); err == nil {
			t.Fatalf("failAt=%d: expected an error", failAt)
		}
	}
}
