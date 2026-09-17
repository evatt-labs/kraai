package plugin

import (
	"errors"
	"testing"

	"go.uber.org/mock/gomock"
)

// TestLoadReadsExactPathAndSurfacesFSError uses the generated MockFS
// rather than mapFS specifically to assert the interaction itself —
// that Host.Load reads exactly spec.Path, once, and propagates whatever
// error the FS returns — the kind of call-shape assertion a hand-rolled
// fake like mapFS (used by this package's other tests, for readability)
// can't express as directly.
func TestLoadReadsExactPathAndSurfacesFSError(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)

	ctrl := gomock.NewController(t)
	fsys := NewMockFS(ctrl)
	wantErr := errors.New("disk on fire")
	fsys.EXPECT().ReadFile("plugins/cost-guard.wasm").Times(1).Return(nil, wantErr)

	_, err := host.Load(ctx, fsys, Spec{Name: "cost-guard", Path: "plugins/cost-guard.wasm"})
	if err == nil {
		t.Fatal("expected Load to surface the FS error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the error chain to include %v, got %v", wantErr, err)
	}
}
