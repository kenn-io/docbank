//go:build !darwin || arm64

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"hegel.dev/go/hegel"
)

// The oracle models one local file, not scan tables or SQL. A successful
// inventory retires it only if this inventory never observed it. Interrupted
// inventories preserve the last durable presence. New scans reset only seen.
func TestNotePublicationModel(t *testing.T) {
	t.Parallel()
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

// The oracle tracks archive liveness, accepted identity and explicit permission,
// rather than deriving eligibility from the implementation's conflict fields.
func TestPublishedSourceLifecycleModel(t *testing.T) {
	t.Parallel()
	hegel.Test(t, func(ht *hegel.T) {
		commands := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 6)))
		s := newTestStore(ht)
		defer func() {
			if err := s.Close(); err != nil {
				ht.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Dir(s.path)); err != nil {
				ht.Fatal(err)
			}
		}()
		scan := beginNoteScan(ht, s)
		a := acceptNote(ht, s, noteUpload(ht, scan, "note.md", fakeHash("ab"), 0))
		accepted, head, revision := a.Source.Identity.ContentVersionID, a.Source.Identity.ContentVersionID, int64(1)
		trashed, available, granted := false, true, false
		for step, command := range commands {
			if step == 32 {
				break
			} // Bound executed I/O, not drawn histories.
			node, err := s.NodeByID(ht.Context(), a.Source.Identity.NodeID)
			if err != nil {
				ht.Fatal(err)
			}
			switch command {
			case 0:
				if trashed {
					break
				}
				_, version, err := s.ReplaceContent(ht.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
				if err != nil {
					ht.Fatal(err)
				}
				head, granted = version.ID, false
			case 1:
				if trashed {
					break
				}
				if _, _, err := s.Trash(ht.Context(), node.ID, node.Revision); err != nil {
					ht.Fatal(err)
				}
				trashed, granted = true, false
			case 2:
				if !trashed {
					break
				}
				if _, _, err := s.Restore(ht.Context(), node.ID, node.Revision); err != nil {
					ht.Fatal(err)
				}
				trashed = false
			case 3:
				if trashed || !available || head == accepted {
					break
				}
				_, err := s.AcceptLocalPublishedNote(ht.Context(), NotePublicationAcceptLocalRequest{Name: "notebook", DatabaseEpoch: scan.DatabaseEpoch, SourceRef: "note.md", ExpectedHeadVersionID: head, ExpectedSourceRevision: revision, OperationID: noteOperationID(ht)})
				if err != nil {
					ht.Fatal(err)
				}
				granted = true
			case 4:
				receipt, err := s.AcceptPublishedNote(ht.Context(), noteUpload(ht, scan, "note.md", fakeHash("ab"), revision))
				if !available {
					if !errors.Is(err, ErrNoteSourceUnavailable) {
						ht.Fatalf("want unavailable, got %v", err)
					}
				} else if trashed || (head != accepted && !granted) {
					if !errors.Is(err, ErrPublishedSourceDiverged) {
						ht.Fatalf("want diverged, got %v", err)
					}
				} else {
					if err != nil {
						ht.Fatal(err)
					}
					if granted {
						revision++
					}
					accepted, head, granted = receipt.Source.Identity.ContentVersionID, receipt.Source.Identity.ContentVersionID, false
				}
			case 5:
				if trashed {
					break
				}
				if _, err := s.PruneContentVersions(ht.Context(), node.ID, node.Revision, VersionPruneSelector{KeepNewest: 1}, true); err != nil {
					ht.Fatal(err)
				}
				if head != accepted {
					available, granted = false, false
				}
			case 6:
				if trashed {
					break
				}
				if _, _, err := s.Move(ht.Context(), node.ID, s.RootID(), fmt.Sprintf("moved-%d.md", step), node.Revision); err != nil {
					ht.Fatal(err)
				}
			}
			source := publishedSource(ht, s, scan)
			wantConflict := ""
			if !available {
				wantConflict = "unavailable"
			} else if trashed {
				wantConflict = "trashed"
			} else if head != accepted {
				wantConflict = "diverged"
			}
			if source.Identity.ContentVersionID != accepted || source.Identity.SourceRevision != revision || source.Identity.SourceRef != "note.md" || source.Conflict != wantConflict || (source.Recovery != nil) != granted {
				ht.Fatalf("step%d command%d expected accepted=%s rev=%d conflict=%s grant=%v; got %+v", step, command, accepted, revision, wantConflict, granted, source)
			}
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(1000))
}
