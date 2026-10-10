package resource

import (
	"context"
	"reflect"
	"testing"
)

func TestEphemeral(t *testing.T) {
	if Ephemeral(context.Background()) {
		t.Fatal("an unmarked context is not ephemeral")
	}
	if !Ephemeral(WithEphemeral(context.Background())) {
		t.Fatal("WithEphemeral's context is ephemeral")
	}
}

// A note reaches the Notes on its context, in order; with none, it is
// dropped rather than panicking.
func TestNotes(t *testing.T) {
	Note(context.Background(), "dropped")
	ctx, notes := WithNotes(context.Background())
	Note(ctx, "first")
	Note(ctx, "second")
	if got := notes.All(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("All = %v", got)
	}
	var none *Notes
	if none.All() != nil {
		t.Fatal("a nil Notes has none")
	}
}
