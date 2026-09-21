// Package kerrors defines kraai's error types. KError wraps a cause built
// with cockroachdb/errors, so a stack is always captured, and tags it with a
// Code that maps to a fixed process exit code. Only cmd/kraai inspects the
// exit code, decides whether to print the stack, and exits.
package kerrors

import (
	"fmt"

	cockroachdb "github.com/cockroachdb/errors"
)

// Code identifies a kraai error's failure category. Each Code has a fixed
// ExitCode; the table is small and CI-branchable on purpose.
type Code int

// Values are explicit, not iota-derived: the exit-code table is a public CI
// contract, so inserting a Code must never shift a downstream exit code.
const (
	// CodeUnexpected is the fallback bucket, including any error that is
	// not a *KError at all.
	CodeUnexpected Code = 1
	// CodeValidation marks a manifest, input or config validation failure.
	CodeValidation Code = 2
	// CodeLockHeld marks an environment lock another operation holds.
	CodeLockHeld Code = 3
	// CodeConfirmationRequired marks a protected operation whose required
	// confirmation was missing or did not match.
	CodeConfirmationRequired Code = 4
)

// ExitCode returns the process exit code for c. The two are the same number
// today; the named conversion is so no call site assumes it.
func (c Code) ExitCode() int {
	return int(c)
}

// String implements fmt.Stringer for readable error and test output.
func (c Code) String() string {
	switch c {
	case CodeUnexpected:
		return "unexpected"
	case CodeValidation:
		return "validation"
	case CodeLockHeld:
		return "lock-held"
	case CodeConfirmationRequired:
		return "confirmation-required"
	default:
		return fmt.Sprintf("kerrors.Code(%d)", int(c))
	}
}

// KError is kraai's base error type. Build one with New, Wrap or a typed
// constructor, never a struct literal, since the cause must be constructed
// through cockroachdb/errors to capture a stack.
type KError struct {
	code  Code
	cause error
}

// Error implements error, returning the message chain and never a stack.
func (e *KError) Error() string {
	return e.cause.Error()
}

// Unwrap exposes the cause so errors.Is and errors.As traverse into it.
func (e *KError) Unwrap() error {
	return e.cause
}

// Format implements fmt.Formatter by delegating to the cause: %v, %s and %q
// print the message chain, %+v adds the captured stack.
func (e *KError) Format(s fmt.State, verb rune) {
	cockroachdb.FormatError(e.cause, s, verb)
}

// Code returns e's failure category.
func (e *KError) Code() Code {
	return e.code
}

// ExitCode returns the process exit code for e.
func (e *KError) ExitCode() int {
	return e.code.ExitCode()
}

// depth skips this file's own constructor frame so the captured stack
// starts at the caller.
const depth = 1

// New creates a *KError in the CodeUnexpected bucket with a formatted
// message. Use a typed constructor when the failure fits a specific bucket.
func New(format string, args ...any) *KError {
	return &KError{code: CodeUnexpected, cause: cockroachdb.NewWithDepthf(depth, format, args...)}
}

// Wrap wraps cause as a *KError with a formatted message and code, for a
// failure that originated outside kraai. A nil cause returns nil.
//
// Wrap returns error, not *KError, so that nil is a true nil interface
// rather than a typed nil whose methods panic. The other constructors never
// return nil and keep *KError, so callers reach Code without an assertion.
func Wrap(cause error, code Code, format string, args ...any) error {
	if cause == nil {
		return nil
	}
	msg := fmt.Sprintf(format, args...)
	return &KError{code: code, cause: cockroachdb.WrapWithDepth(depth, cause, msg)}
}

// Validation creates a *KError in the CodeValidation bucket.
func Validation(format string, args ...any) *KError {
	return &KError{code: CodeValidation, cause: cockroachdb.NewWithDepthf(depth, format, args...)}
}

// LockHeld creates a *KError in the CodeLockHeld bucket.
func LockHeld(format string, args ...any) *KError {
	return &KError{code: CodeLockHeld, cause: cockroachdb.NewWithDepthf(depth, format, args...)}
}

// ConfirmationRequired creates a *KError in the CodeConfirmationRequired
// bucket.
func ConfirmationRequired(format string, args ...any) *KError {
	return &KError{code: CodeConfirmationRequired, cause: cockroachdb.NewWithDepthf(depth, format, args...)}
}

// ExitCode maps err to a process exit code: 0 for nil, the *KError's own
// code when one is anywhere in the chain, and CodeUnexpected's otherwise.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var kerr *KError
	if cockroachdb.As(err, &kerr) {
		return kerr.ExitCode()
	}
	return CodeUnexpected.ExitCode()
}
