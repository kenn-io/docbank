package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func publishDOCXFingerprint(t *testing.T, s *Store, hash, fingerprint string) {
	t.Helper()
	encoded, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{{Key: "office.package.docx_sha256_v1",
			Namespace: "office.package", SourceField: "zip entries",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: &fingerprint}}},
	})
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(t.Context(), hash, fakeHash("ee"), encoded)
	require.NoError(t, err)
}

func TestDOCXDuplicatesPreserveIdentityAndScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	const mime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	a, err := s.CreateFile(ctx, s.RootID(), "first.docx", fakeHash("11"), 100, mime)
	require.NoError(t, err)
	b, err := s.CreateFile(ctx, s.RootID(), "second.docx", fakeHash("22"), 110, mime)
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "waiting.docx", fakeHash("33"), 80, mime)
	require.NoError(t, err)
	publishDOCXFingerprint(t, s, a.BlobHash, fakeHash("aa"))
	publishDOCXFingerprint(t, s, b.BlobHash, fakeHash("aa"))
	page, err := s.DOCXDuplicates(ctx, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, page.Total)
	require.Equal(t, 2, page.TotalReferences)
	require.Equal(t, 1, page.UnfingerprintedReferences)
	require.Len(t, page.Items, 1)
	require.Equal(t, fakeHash("aa"), page.Items[0].Fingerprint)
	require.Len(t, page.Items[0].References, 2)
	require.Equal(t, a, page.Items[0].References[0].Node)
	require.Equal(t, b, page.Items[0].References[1].Node)
	require.Equal(t, "/first.docx", page.Items[0].References[0].Path)
	exact, err := s.Duplicates(ctx, 10, 0)
	require.NoError(t, err)
	require.Empty(t, exact.Items)
	_, _, err = s.ReplaceContent(ctx, b.ID, b.Revision, fakeHash("44"), 120, mime)
	require.NoError(t, err)
	page, err = s.DOCXDuplicates(ctx, 10, 0)
	require.NoError(t, err)
	require.Empty(t, page.Items, "historical content must not form a current group")
	c, err := s.CreateFile(ctx, s.RootID(), "third.docx", a.BlobHash, a.Size, mime)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, c.ID, c.Revision)
	require.NoError(t, err)
	page, err = s.DOCXDuplicates(ctx, 10, 0)
	require.NoError(t, err)
	require.Empty(t, page.Items, "trash must not form a current group")
}

func TestDOCXDuplicatesBoundsAndCorruptEvidence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	for group := range 2 {
		for i := range 17 {
			hash := fakeHash(fmt.Sprintf("%02x", 1+group*17+i))
			_, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("copy-%d-%d.docx", group, i), hash, 10,
				"application/vnd.openxmlformats-officedocument.wordprocessingml.document")
			require.NoError(t, err)
			publishDOCXFingerprint(t, s, hash, fakeHash(fmt.Sprintf("%02x", 128+group)))
		}
	}
	page, err := s.DOCXDuplicates(ctx, 1, 1)
	require.NoError(t, err)
	require.Equal(t, 2, page.Total)
	require.Equal(t, 34, page.TotalReferences)
	require.Len(t, page.Items, 1)
	require.Equal(t, fakeHash("81"), page.Items[0].Fingerprint)
	require.Equal(t, 17, page.Items[0].ReferenceCount)
	require.Len(t, page.Items[0].References, 16)
	require.True(t, page.Items[0].ReferencesTruncated)
	page, err = s.DOCXDuplicates(ctx, 1, 20)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Equal(t, 2, page.Total)
	_, err = s.DOCXDuplicates(ctx, 101, 0)
	require.ErrorIs(t, err, ErrInvalidDuplicatePage)
	_, err = s.db.ExecContext(ctx, `DROP TRIGGER source_metadata_generations_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE source_metadata_generations SET checksum=? WHERE source_sha256=?`, fakeHash("ff"), fakeHash("01"))
	require.NoError(t, err)
	_, err = s.DOCXDuplicates(ctx, 10, 0)
	require.ErrorIs(t, err, ErrSourceMetadataCorrupt, "never group unverified evidence")
}
