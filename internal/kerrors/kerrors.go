// Package kerrors defines kraai's error types.
//
// KError is the base error type: it wraps a cause built with
// github.com/cockroachdb/errors (so stack capture is never hand-rolled) and
// tags it with a Code, which maps to a fixed process exit code. Every other
// package returns errors built with the constructors in this file; only
// cmd/kraai's centralized handler inspects a Code's ExitCode(), decides
// whether to print the stack (%+v) or just the message chain, and calls
// os.Exit.
package kerrors

import (
	"fmt"

	cockroachdb "github.com/cockroachdb/errors"
)

// Code identifies a kraai error's failure category. Each Code has a fixed
// ExitCode() — the table is small and CI-branchable on purpose, not one
// code per Go error type.
type Code int

// Values are explicit, not iota-derived: the exit-code table is a
// documented public CI contract, so inserting a new Code between existing
// ones must never silently shift a downstream exit code.
const (
	// CodeUnexpected is the fallback bucket for generic/unrecognized
	// errors, including any error that isn't a *KError at all.
	CodeUnexpected Code = 1
	// CodeValidation marks a manifest/input/config validation failure.
	CodeValidation Code = 2
	// CodeLockHeld marks a failure to acquire an environment lock because
	// another operation already holds it.
	CodeLockHeld Code = 3
	// CodeConfirmationRequired marks a protected operation whose required
	// confirmation was missing or didn't match.
	CodeConfirmationRequired Code = 4
)

// ExitCode returns the process exit code for c.
// The numeric value of Code and its ExitCode are deliberately the same
// today; ExitCode exists as the named, documented conversion so the two
// don't need to be assumed identical at every call site.
func (c Code) ExitCode() int {
	return int(c)
}

// String implements fmt.Stringer for readable error/test output.
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

// KError is kraai's base error type. Build one with New, Wrap, or one of the
// typed constructors (Validation, LockHeld, ConfirmationRequired) below —
// never with a struct literal, since the cause must be constructed through
// cockroachdb/errors to capture a stack trace.
type KError struct {
	code  Code
	cause error
}

// Error implements error. It returns the wrapped message chain (e.g.
// "outer: inner"), never a stack trace — that's only ever included via
// Format's %+v handling, for cmd/kraai's debug-mode printing.
func (e *KError) Error() string {
	return e.cause.Error()
}

// Unwrap exposes the wrapped cause so stdlib errors.Is/errors.As/
// errors.Unwrap traverse into it, and so kerrors errors compose as
// drop-in-compatible stdlib errors.
func (e *KError) Unwrap() error {
	return e.cause
}

// Format implements fmt.Formatter by delegating to the cause's own
// Formatter, which cockroachdb/errors always provides: %v/%s/%q print the
// message chain, %+v additionally prints the captured stack trace. KError
// never captures or formats a stack itself.
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
// starts at the caller of New/Wrap/Validation/etc., not inside kerrors.
const depth = 1

// New creates a *KError in the generic/unexpected bucket (CodeUnexpected)
// with a formatted message. Use a typed constructor below instead when the
// failure fits one of the specific buckets below.
func New(format string, args ...any) *KError {
	return &KError{code: CodeUnexpected, cause: cockroachdb.NewWithDepthf(depth, format, args...)}
}

// Wrap wraps cause as a *KError, adding a formatted message and assigning
// it code. Use this to attach one of the buckets above to a failure that
// originated outside kraai (a cloud SDK error, an os error, etc). If cause
// is nil, Wrap returns nil, matching the fmt.Errorf/errors.Wrap convention of
// being a no-op wrapper around a non-error.
//
// Wrap returns the error interface, not *KError, deliberately: a *KError
// return type would make Wrap(nil, ...) a classic Go typed-nil trap — a
// caller doing `return kerrors.Wrap(cause, ...)` from a function returning
// error would get back a non-nil error interface holding a nil *KError,
// and both ExitCode() and Error() would then panic on the nil receiver.
// Returning error here means the nil case below converts to a true nil
// interface. The other constructors below never return nil, so they keep
// *KError, which lets callers reach Code()/ExitCode() without a type
// assertion.
func Wrap(cause error, code Code, format string, args ...any) error {
	if cause == nil {
		return nil
	}
	msg := fmt.Sprintf(format, args...)
	return &KError{code: code, cause: cockroachdb.WrapWithDepth(depth, cause, msg)}
}

// Validation creates a *KError in the CodeValidation bucket: a
// manifest/input/config value failed validation.
func Validation(format string, args ...any) *KError {
	return &KError{code: CodeValidation, cause: cockroachdb.NewWithDepthf(depth, format, args...)}
}

// LockHeld creates a *KError in the CodeLockHeld bucket: an environment
// lock is already held by another operation.
func LockHeld(format string, args ...any) *KError {
	return &KError{code: CodeLockHeld, cause: cockroachdb.NewWithDepthf(depth, format, args...)}
}

// ConfirmationRequired creates a *KError in the CodeConfirmationRequired
// bucket: a protected operation's required confirmation was missing or
// didn't match.
func ConfirmationRequired(format string, args ...any) *KError {
	return &KError{code: CodeConfirmationRequired, cause: cockroachdb.NewWithDepthf(depth, format, args...)}
}

// ExitCode maps err to the process exit code cmd/kraai's centralized
// handler should use: a nil err is success (0), a *KError anywhere in
// err's chain yields its own ExitCode(), and anything else falls back to
// CodeUnexpected's exit code (1).
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
