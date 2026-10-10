package resource

import (
	"context"
	"sync"
)

type ephemeralKey struct{}

// WithEphemeral marks ctx as a run against an ephemeral environment, whose
// data nothing keeps once it is destroyed: a delete that would otherwise
// leave a copy behind, such as a database's final snapshot, leaves none.
func WithEphemeral(ctx context.Context) context.Context {
	return context.WithValue(ctx, ephemeralKey{}, true)
}

// Ephemeral reports whether WithEphemeral marked ctx.
func Ephemeral(ctx context.Context) bool {
	ephemeral, _ := ctx.Value(ephemeralKey{}).(bool)
	return ephemeral
}

// Notes collects what a mutation tells the operator beyond its outcome,
// such as the name of a snapshot a delete kept. Safe for concurrent use.
type Notes struct {
	mu    sync.Mutex
	notes []string
}

// All returns the notes collected, in the order they were added.
func (n *Notes) All() []string {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.notes...)
}

type notesKey struct{}

// WithNotes returns ctx carrying a fresh Notes, which Note adds to.
func WithNotes(ctx context.Context) (context.Context, *Notes) {
	notes := &Notes{}
	return context.WithValue(ctx, notesKey{}, notes), notes
}

// Note adds note to the Notes WithNotes put on ctx; without one, nothing.
func Note(ctx context.Context, note string) {
	notes, _ := ctx.Value(notesKey{}).(*Notes)
	if notes == nil {
		return
	}
	notes.mu.Lock()
	defer notes.mu.Unlock()
	notes.notes = append(notes.notes, note)
}
