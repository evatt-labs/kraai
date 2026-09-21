package naming

import (
	cryptorand "crypto/rand"
	"math/big"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// randReader is the source randomIntn reads from: a package var only so
// tests can inject a failing reader, since crypto/rand.Reader never fails.
var randReader = cryptorand.Reader

// randomIntn returns a uniformly distributed integer in [0, n), from
// crypto/rand. Predictability is not a security property of an environment
// name, but a CSPRNG costs nothing and keeps gosec from flagging every sink
// the name flows into.
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

// drawInt is randomIntn, as a package var so a test can make one specific
// draw among GenerateEnvironmentName's several fail.
var drawInt = randomIntn
