//go:build !darwin || arm64

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"hegel.dev/go/hegel"
)

// The oracle models one local file, not scan tables or SQL. A successful
// inventory retires it only if this inventory never observed it. Interrupted
// inventories preserve the last durable presence. New scans reset only seen.
func TestNotePublicationModel(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		commands := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 5)))
		s := newTestStore(ht)
		defer func() {
			if err := s.Close(); err != nil {
				ht.Fatalf("closing synthetic case: %v", err)
			}
			if err := os.RemoveAll(filepath.Dir(s.path)); err != nil {
				ht.Fatalf("removing synthetic case: %v", err)
			}
		}()
		scan := beginNoteScan(ht, s)
		var binding PublishedNoteIdentity
		var hash string
		known, present, seen, sealed := false, false, false, false
		for step, command := range commands {
			if step == 64 {
				break
			} // bound executed I/O, not the drawn history domain
			before, err := s.NotePublicationStatus(t.Context(), "notebook")
			if err != nil {
				ht.Fatalf("step %d: status: %v", step, err)
			}
			changed := false
			switch command {
			case 0:
				scan = beginNoteScan(ht, s)
				seen, sealed = false, false
			case 1, 2:
				if sealed {
					break
				}
				nextHash := fakeHash("bc")
				if command == 2 {
					nextHash = fakeHash("cd")
				}
				changed = !known || hash != nextHash || !present
				receipt := acceptNote(ht, s, noteUpload(ht, scan, "note.md", nextHash, binding.SourceRevision))
				binding = receipt.Source.Identity
				hash, known, present, seen = nextHash, true, true, true
			case 3:
				if sealed || !known {
					break
				}
				_, err = s.AcknowledgePublishedNotes(t.Context(), NotePublicationSeenRequest{Name: "notebook", ScanID: scan.ScanID, Bindings: []PublishedNoteIdentity{binding}})
				if err != nil {
					ht.Fatalf("step %d: seen: %v", step, err)
				}
				changed = !present
				present, seen = true, true
			case 4:
				completeNoteScan(ht, s, scan)
				if !sealed && known && !seen && present {
					present, changed = false, true
				}
				sealed = true
			case 5:
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				_, err = s.CompleteNotePublicationScan(ctx, NotePublicationCompleteRequest{Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true})
				if err == nil {
					ht.Fatalf("step %d: cancellation committed", step)
				}
			}
			after, err := s.NotePublicationStatus(t.Context(), "notebook")
			if err != nil {
				ht.Fatalf("step %d: status: %v", step, err)
			}
			if (after.NamespaceRevision > before.NamespaceRevision) != changed {
				ht.Fatalf("step %d command %d: semantic change=%v, revisions %d→%d", step, command, changed, before.NamespaceRevision, after.NamespaceRevision)
			}
			wantPresent, wantAbsent := int64(0), int64(0)
			if known {
				if present {
					wantPresent = 1
				} else {
					wantAbsent = 1
				}
			}
			if after.Present != wantPresent || after.Absent != wantAbsent || after.Incomplete == sealed {
				ht.Fatalf("step %d command %d: want present=%d absent=%d sealed=%v; got %+v", step, command, wantPresent, wantAbsent, sealed, after)
			}
			if known {
				page, err := s.NotePublicationSources(t.Context(), NotePublicationSourcesRequest{Name: "notebook", ScanID: scan.ScanID, Limit: 10})
				if err != nil || len(page.Items) != 1 || page.Items[0].Identity.Hash != hash {
					ht.Fatalf("step %d: source mismatch: %+v %v", step, page, err)
				}
				binding = page.Items[0].Identity
			}
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(1000))
}
