package loadfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestPreflightRefusesEveryHostileReference(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "a.pdf"), []byte("%PDF-1.7\n"), 0o600))
	resolver, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	volume := Volume{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}
	for name, relPath := range map[string]string{
		"traversal": "../../etc/passwd",
		"absolute":  "/etc/passwd",
		"url":       "https://example.test/a.pdf",
		"casefold":  "A.PDF",
		"reserved":  "AUX.txt",
		"ads":       "a.pdf:secret",
	} {
		_, err = resolver.Resolve(volume, relPath)
		require.ErrorIs(t, err, ErrUnsafeReference, name)
	}
	_, err = resolver.Resolve(Volume{Name: "VOL999"}, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
	resolved, err := resolver.Resolve(volume, "a.pdf")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "VOL001", "a.pdf"), resolved)
	digest, err := resolver.RootDigest()
	require.NoError(t, err)
	assert.True(t, canonical.IsSHA256Hex(digest))
	assert.NotContains(t, digest, root)
}

func TestValidateReportsBrokenFamilyDuplicateIDAndPageMismatch(t *testing.T) {
	diagnostics, err := Validate(t.Context(), abPackageWithFaults(t))
	require.NoError(t, err)
	codes := diagnosticCodes(diagnostics)
	assert.Contains(t, codes, "family_edge_unresolved")
	assert.Contains(t, codes, "duplicate_document_id")
	assert.Contains(t, codes, "page_count_mismatch")
	assert.Contains(t, codes, "image_missing")
	assert.True(t, Blocking(diagnostics))
}

func TestAuthorizedVolumeRootRemappingIsExplicitAndRecorded(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "DELIVERY", "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "DELIVERY", "VOL001", "a.pdf"), []byte("%PDF-1.7\n"), 0o600))
	volume := Volume{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}

	plain, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plain.Close()) })
	_, err = plain.Resolve(volume, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
	assert.Empty(t, plain.Remappings())

	remapped, err := NewResolver(root, map[string]string{"VOL001": "DELIVERY/VOL001"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remapped.Close()) })
	resolved, err := remapped.Resolve(volume, "a.pdf")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "DELIVERY", "VOL001", "a.pdf"), resolved)
	require.Equal(t, []VolumeRemap{{VolumeName: "VOL001", DeclaredRoot: "VOL001", MappedRoot: "DELIVERY/VOL001"}}, remapped.Remappings())

	_, err = NewResolver(root, map[string]string{"VOL001": "../elsewhere"})
	require.ErrorIs(t, err, ErrUnsafeReference)

	unused, err := NewResolver(root, map[string]string{"VOL001": "DELIVERY/VOL001"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unused.Close()) })
	_, err = unused.Resolve(volume, "missing.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
	assert.Empty(t, unused.Remappings(), "failed paths do not count as applied remaps")
}

func TestValidateReconcilesImageIdentityChildBoundariesAndFamilyCycles(t *testing.T) {
	root := syntheticRoot(t)
	resolver, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	diagnostics, err := Validate(t.Context(), ValidateInput{
		Records: []Record{
			{RowID: "row-a", DocID: "DOC-A", Family: Family{ParentDocID: "DOC-B"}},
			{RowID: "row-b", DocID: "DOC-B", Family: Family{AttachmentDocIDs: []string{"DOC-A"}, ParentDocID: "DOC-A"}},
			{RowID: "row-c", DocID: "DOC-C"},
		},
		Images: []ImageRef{
			{ImageKey: "DOC-C", Volume: "VOL001", RelPath: "IMAGES/001/DOC-A-1.tif", DocumentBreak: true, Boundary: "child", PageOrdinal: 1},
			{ImageKey: "DOC-UNKNOWN", Volume: "VOL001", RelPath: "IMAGES/001/DOC-B-1.tif", DocumentBreak: true, Boundary: "document", PageOrdinal: 1},
		},
		Volumes:  []Volume{{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}},
		Resolver: resolver,
	})
	require.NoError(t, err)
	codes := diagnosticCodes(diagnostics)
	assert.Contains(t, codes, "image_identity_unresolved")
	assert.Contains(t, codes, "family_edge_unresolved")
	assert.Contains(t, codes, "family_cycle")
}

func TestValidateRejectsPageMapWhenEveryDocumentIdentityIsUnknown(t *testing.T) {
	root := syntheticRoot(t)
	resolver, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	diagnostics, err := Validate(t.Context(), ValidateInput{
		Records:  []Record{{RowID: "row-a", DocID: "DOC-A"}},
		Images:   []ImageRef{{ImageKey: "UNKNOWN", Volume: "VOL001", RelPath: "IMAGES/001/DOC-A-1.tif", DocumentBreak: true, Boundary: "child", PageOrdinal: 1}},
		Volumes:  []Volume{{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}},
		Resolver: resolver,
	})
	require.NoError(t, err)
	assert.Contains(t, diagnosticCodes(diagnostics), "image_identity_unresolved")
}

func TestFamilyCycleDiagnosticsAreDeterministicAndNameOnlyCycleMembers(t *testing.T) {
	records := []Record{
		{RowID: "row-d", DocID: "D", Family: Family{ParentDocID: "A"}},
		{RowID: "row-b", DocID: "B", Family: Family{ParentDocID: "A"}},
		{RowID: "row-a", DocID: "A", Family: Family{ParentDocID: "B"}},
	}
	first := familyCycleDiagnostics(records)
	second := familyCycleDiagnostics(records)
	require.Equal(t, first, second)
	require.Len(t, first, 2)
	assert.Equal(t, []string{"row-a", "row-b"}, []string{first[0].RowID, first[1].RowID})
}
