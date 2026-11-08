//go:build !darwin || arm64

package store

import (
	"testing"

	"hegel.dev/go/hegel"
)

func TestPublishedNoteValidationProperty(t *testing.T) {
	t.Parallel()
	hegel.Test(t, func(ht *hegel.T) {
		ref := hegel.Draw(ht, hegel.Text())
		source := NotePublicationSource{Identity: PublishedNoteIdentity{VaultID: metadataIngestID,
			Publication: "notebook", SourceRef: ref, NodeID: 2, ContentVersionID: metadataVersionCurrent,
			Hash: fakeHash("ab"), Size: 4, SourceRevision: 1, NamespaceRevision: 1,
		}, Presence: "present", Availability: "retained"}
		err := ValidatePublishedNoteSource(source)
		if (err == nil) != (ValidatePushRef(ref) == nil) {
			ht.Fatalf("source ref validation differs for %q: %v", ref, err)
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(2000))
}
