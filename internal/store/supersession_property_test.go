//go:build !darwin || arm64

package store

import (
	"fmt"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
)

// Hegel ships native engines on macOS arm64 but not macOS amd64. The
// deterministic supersession regressions still run on every supported target.
func TestSupersessionOmittedNoteAfterSuccessorTrashRestoreProperty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	sequence := 0
	hegel.Test(t, func(ht *hegel.T) {
		iteration := sequence
		sequence++
		noteLength := hegel.Draw(ht, hegel.Integers(1, maxSupersessionNote))
		note := strings.Repeat("n", noteLength)
		old, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("old-%d.txt", iteration), fakeHash(fmt.Sprintf("old-%d", iteration)), 1, "text/plain")
		if err != nil {
			ht.Fatalf("create predecessor: %v", err)
		}
		next, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("next-%d.txt", iteration), fakeHash(fmt.Sprintf("next-%d", iteration)), 1, "text/plain")
		if err != nil {
			ht.Fatalf("create successor: %v", err)
		}
		created, err := s.PutSupersession(ctx, next.ID, old.ID, old.Revision, &note)
		if err != nil {
			ht.Fatalf("create supersession: %v", err)
		}
		trashed, _, err := s.Trash(ctx, next.ID, next.Revision)
		if err != nil {
			ht.Fatalf("trash successor: %v", err)
		}
		staleView, err := s.NodeValidity(ctx, old.ID, "2025-01-01")
		if err != nil {
			ht.Fatalf("read predecessor while successor is trashed: %v", err)
		}
		if len(staleView.Successors) != 0 {
			ht.Fatalf("trashed successor remained visible: %+v", staleView.Successors)
		}
		restored, _, err := s.Restore(ctx, trashed.ID, trashed.Revision)
		if err != nil {
			ht.Fatalf("restore successor: %v", err)
		}
		if _, err := s.PutSupersession(ctx, restored.ID, old.ID, created.Node.Revision, nil); err != nil {
			ht.Fatalf("write supersession after successor restore: %v", err)
		}
		currentView, err := s.NodeValidity(ctx, old.ID, "2025-01-01")
		if err != nil {
			ht.Fatalf("read restored supersession: %v", err)
		}
		if len(currentView.Successors) != 1 || currentView.Successors[0].Note != note {
			ht.Fatalf("omitted note lost across trash/restore: got %+v, want note %q", currentView.Successors, note)
		}
	}, hegel.WithDatabase(t.TempDir()))
}
