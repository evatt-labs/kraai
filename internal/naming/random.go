package naming

import (
	cryptorand "crypto/rand"
	"math/big"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// randReader is the CSPRNG source randomIntn reads from. It's a package
// var, not a hardcoded reference to crypto/rand.Reader, only so this
// package's own tests can inject a failing reader to exercise the read
// error path — crypto/rand.Reader itself doesn't fail on any platform Go
// supports, so there'd otherwise be no way to test it at all. Every real
// caller always gets crypto/rand.Reader; nothing outside this package's
// tests ever reassigns it.
var randReader = cryptorand.Reader

// randomIntn returns a uniformly-distributed integer in [0, n), backed by
// crypto/rand rather than math/rand. This mirrors Node's
// crypto.randomInt, which the JavaScript CLI used for the same
// reason (see its own comment on pick()): a generated environment name
// reaches a workers.dev hostname and, per D27, a path on disk. Neither is
// a secret and predictability isn't a security property here, but a
// CSPRNG costs nothing and keeps gosec's insecure-randomness rule from
// flagging every sink the name flows into.
func randomIntn(n int) (int64, error) {
	if n <= 0 {
		return 0, kerrors.New("naming: randomIntn requires a positive bound, got %d", n)
	}
	v, err := cryptorand.Int(randReader, big.NewInt(int64(n)))
	if err != nil {
		return 0, kerrors.Wrap(err, kerrors.CodeUnexpected, "naming: reading random bytes")
	}
	return v.Int64(), nil
}

// drawInt is randomIntn by default. It exists as a package var purely so
// GenerateEnvironmentName's own tests can make one specific draw among
// its several fail deterministically: crypto/rand's internal Read call
// pattern isn't a stable enough seam to target "the third draw fails"
// directly. Every real caller always gets randomIntn itself; nothing
// outside this package's tests ever reassigns it.
var drawInt = randomIntn
