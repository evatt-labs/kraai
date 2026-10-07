// Package redact keeps the sensitive values one command has seen out of
// everything it prints: its output, its error and the errors its spans
// record. A value is added when it is learned, such as a sensitive
// Terraform output read with the manifest, and replaced by a label naming
// where it came from wherever it then appears.
//
// Replacement is by string, as Terraform's own redaction of its logs is: a
// value transformed before it is printed, such as one base64-encoded, is
// not recognised, and one shorter than minLength is not replaced, since
// replacing a few characters everywhere would mangle the output without
// protecting anything.
package redact

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// minLength is the shortest value replaced.
const minLength = 4

// Set is the sensitive values one command has learned. The zero value is
// ready to use, and a nil Set redacts nothing.
type Set struct {
	mu     sync.RWMutex
	labels map[string]string
	// replacer is rebuilt on Add, longest value first, so a value that
	// contains another is replaced whole.
	replacer *strings.Replacer
}

// Add records value as sensitive, to be printed as [sensitive label].
func (s *Set) Add(value, label string) {
	if s == nil || len(value) < minLength {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.labels == nil {
		s.labels = map[string]string{}
	}
	s.labels[value] = label
	values := make([]string, 0, len(s.labels))
	for v := range s.labels {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	pairs := make([]string, 0, 2*len(values))
	for _, v := range values {
		pairs = append(pairs, v, "[sensitive "+s.labels[v]+"]")
	}
	s.replacer = strings.NewReplacer(pairs...)
}

// String returns in with every sensitive value replaced.
func (s *Set) String(in string) string {
	if s == nil {
		return in
	}
	s.mu.RLock()
	r := s.replacer
	s.mu.RUnlock()
	if r == nil {
		return in
	}
	return r.Replace(in)
}

// Error returns err with its message, and its %+v form, redacted; nil for
// nil. errors.Is and errors.As still see the original, so its exit code
// survives.
func (s *Set) Error(err error) error {
	if err == nil {
		return nil
	}
	return &redacted{err: err, set: s}
}

type redacted struct {
	err error
	set *Set
}

func (r *redacted) Error() string { return r.set.String(r.err.Error()) }
func (r *redacted) Unwrap() error { return r.err }

// Format redacts every verb's rendering, including the stack trace %+v
// prints under --debug.
func (r *redacted) Format(f fmt.State, verb rune) {
	format := "%" + string(verb)
	if f.Flag('+') {
		format = "%+" + string(verb)
	}
	_, _ = io.WriteString(f, r.set.String(fmt.Sprintf(format, r.err)))
}

// Writer returns w with every write redacted. A value split across two
// writes is not recognised; kraai prints each line in one write.
func (s *Set) Writer(w io.Writer) io.Writer { return writer{w: w, set: s} }

type writer struct {
	w   io.Writer
	set *Set
}

func (w writer) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.w, w.set.String(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

type key struct{}

// With returns ctx carrying s.
func With(ctx context.Context, s *Set) context.Context {
	return context.WithValue(ctx, key{}, s)
}

// From returns the Set ctx carries, or nil, which redacts nothing.
func From(ctx context.Context) *Set {
	s, _ := ctx.Value(key{}).(*Set)
	return s
}
