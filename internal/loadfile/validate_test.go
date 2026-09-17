package loadfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestValidateCountsCapturedPDFWhenSourceChanges(t *testing.T) {
	pdfapi.DisableConfigDir()
	for _, change := range []string{"overwrite", "remove"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
			original, err := os.ReadFile("../../document/testdata/scanassessment/blank.pdf")
			require.NoError(t, err)
			path := filepath.Join(root, "VOL001", "source.pdf")
			require.NoError(t, os.WriteFile(path, original, 0o600))
			resolver, err := NewResolver(t.Context(), root, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, resolver.Close()) })
			snapshotDir := t.TempDir()
			for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(variable, snapshotDir)
			}
			files, diagnostics, err := Validate(t.Context(), ValidateInput{
				Resolver: resolver, Volumes: []Volume{{Name: "VOL001", DeclaredRoot: "VOL001"}},
				Records: []Record{{DocID: "DOC-A", Files: []FileRef{{Role: "native", Volume: "VOL001", RelPath: "source.pdf"}}}},
				PageCount: func(file io.ReadSeeker) (int, error) {
					if change == "overwrite" {
						require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), len(original)), 0o600))
					} else {
						require.NoError(t, os.Remove(path))
					}
					pages, countErr := pdfapi.PageCount(file, nil)
					require.NoError(t, countErr)
					assert.Equal(t, 1, pages)
					return pages, nil
				},
			})
			require.NoError(t, err)
			assert.Empty(t, diagnostics)
			require.Len(t, files, 1)
			assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(original)), files[0].SHA256)
			assert.Equal(t, int64(len(original)), files[0].Size)
			remaining, err := os.ReadDir(snapshotDir)
			require.NoError(t, err)
			assert.Empty(t, remaining, "PDF snapshots must be removed after validation")
		})
	}
}

func TestValidateCancelsPDFSnapshotReadsAndCleansUp(t *testing.T) {
	input := abPackageWithFaults(t)
	snapshotDir := t.TempDir()
	for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(variable, snapshotDir)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input.PageCount = func(file io.ReadSeeker) (int, error) {
		cancel()
		_, err := file.Read(make([]byte, 1))
		require.ErrorIs(t, err, context.Canceled)
		_, err = file.Seek(0, io.SeekStart)
		require.ErrorIs(t, err, context.Canceled)
		return 0, err
	}
	_, _, err := Validate(ctx, input)
	require.ErrorIs(t, err, context.Canceled)
	remaining, err := os.ReadDir(snapshotDir)
	require.NoError(t, err)
	assert.Empty(t, remaining, "canceled PDF validation must remove its snapshot")
}

func TestPreflightRefusesEveryHostileReference(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "a.pdf"), []byte("%PDF-1.7\n"), 0o600))
	resolver, err := NewResolver(t.Context(), root, nil)
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
		_, err = resolver.Open(volume, relPath)
		require.ErrorIs(t, err, ErrUnsafeReference, name)
	}
	_, err = resolver.Open(Volume{Name: "VOL999"}, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
	resolved, err := resolver.Open(volume, "a.pdf")
	require.NoError(t, err)
	require.NoError(t, resolved.Close())
	digest, err := resolver.RootDigest()
	require.NoError(t, err)
	assert.True(t, canonical.IsSHA256Hex(digest))
	assert.NotContains(t, digest, root)
}

func TestValidateReportsBrokenFamilyDuplicateIDAndPageMismatch(t *testing.T) {
	_, diagnostics, err := Validate(t.Context(), abPackageWithFaults(t))
	require.NoError(t, err)
	codes := diagnosticCodes(diagnostics)
	assert.Contains(t, codes, "family_edge_unresolved")
	assert.Contains(t, codes, "duplicate_document_id")
	assert.Contains(t, codes, "page_count_mismatch")
	assert.Contains(t, codes, "image_missing")
	assert.True(t, Blocking(diagnostics))
}

func TestValidateRejectsSourceGrowth(t *testing.T) {
	for _, role := range []string{"native", "page_image"} {
		t.Run(role, func(t *testing.T) {
			root := syntheticRoot(t)
			resolver, err := NewResolver(t.Context(), root, nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, resolver.Close()) }()
			require.NoError(t, os.Truncate(filepath.Join(root, "VOL001", "NATIVES", "DOC-A.pdf"), 1024))
			input := ValidateInput{Resolver: resolver, Volumes: []Volume{{Name: "VOL001", DeclaredRoot: "VOL001"}}}
			if role == "native" {
				input.Records = []Record{{DocID: "DOC-A", Files: []FileRef{{Role: role, Volume: "VOL001", RelPath: "NATIVES/DOC-A.pdf", Status: "available"}}}}
			} else {
				input.Images = []ImageRef{{Volume: "VOL001", RelPath: "NATIVES/DOC-A.pdf"}}
			}
			_, _, err = Validate(t.Context(), input)
			require.ErrorIs(t, err, ErrMalformedInput)
		})
	}
}

func TestValidateHashesFilesAndRejectsCancellation(t *testing.T) {
	root := syntheticRoot(t)
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	input := ValidateInput{
		Resolver: resolver, Volumes: []Volume{{Name: "VOL001", DeclaredRoot: "VOL001"}},
		Records: []Record{{DocID: "DOC-A", Files: []FileRef{{Role: "text", Volume: "VOL001", RelPath: "TEXT/DOC-A.txt", Declared: `TEXT\DOC-A.txt`}}}},
	}
	files, diagnostics, err := Validate(t.Context(), input)
	require.NoError(t, err)
	require.Empty(t, diagnostics)
	require.Len(t, files, 1)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic text A"))), files[0].SHA256)
	assert.Equal(t, int64(len("synthetic text A")), files[0].Size)
	assert.Equal(t, `TEXT\DOC-A.txt`, files[0].Declared)
	assert.Equal(t, files[0], input.Records[0].Files[0])
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = Validate(ctx, input)
	require.ErrorIs(t, err, context.Canceled)
}

func TestValidateBlocksReferencesRemovedBeforeCapture(t *testing.T) {
	root := syntheticRoot(t)
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	input := ValidateInput{
		Resolver: resolver, Volumes: []Volume{{Name: "VOL001", DeclaredRoot: "VOL001"}},
		Records: []Record{{DocID: "DOC-A", RowID: "row-a", RowOrdinal: 2, Files: []FileRef{
			{Role: "native", Volume: "VOL001", RelPath: "NATIVES/DOC-A.pdf"},
			{Role: "text", Volume: "VOL001", RelPath: "TEXT/DOC-A.txt"},
		}}},
		Images: []ImageRef{{ImageKey: "DOC-A", Volume: "VOL001", RelPath: "IMAGES/001/DOC-A-1.tif", DocumentBreak: true}},
		PageCount: func(io.ReadSeeker) (int, error) {
			require.NoError(t, os.Remove(filepath.Join(root, "VOL001", "TEXT", "DOC-A.txt")))
			require.NoError(t, os.Remove(filepath.Join(root, "VOL001", "IMAGES", "001", "DOC-A-1.tif")))
			return 1, nil
		},
	}
	files, diagnostics, err := Validate(t.Context(), input)
	require.NoError(t, err)
	require.Len(t, files, 3)
	assert.Equal(t, "missing", files[1].Status)
	assert.Equal(t, "missing", files[2].Status)
	assert.Equal(t, files[1], input.Records[0].Files[1])
	require.Len(t, diagnostics, 2)
	assert.True(t, Blocking(diagnostics))
	assert.Equal(t, []string{"file_missing", "image_missing"}, diagnosticCodes(diagnostics))
	assert.Equal(t, "row-a", diagnostics[0].RowID)
	assert.Equal(t, "DOC-A", diagnostics[1].RowID)
}

func TestAuthorizedVolumeRootRemappingIsExplicit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "DELIVERY", "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "DELIVERY", "VOL001", "a.pdf"), []byte("%PDF-1.7\n"), 0o600))
	volume := Volume{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}

	plain, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plain.Close()) })
	_, err = plain.Open(volume, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)

	remapped, err := NewResolver(t.Context(), root, map[string]string{"VOL001": "DELIVERY/VOL001"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remapped.Close()) })
	resolved, err := remapped.Open(volume, "a.pdf")
	require.NoError(t, err)
	require.NoError(t, resolved.Close())

	_, err = NewResolver(t.Context(), root, map[string]string{"VOL001": "../elsewhere"})
	require.ErrorIs(t, err, ErrUnsafeReference)

	unused, err := NewResolver(t.Context(), root, map[string]string{"VOL001": "DELIVERY/VOL001"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unused.Close()) })
	_, err = unused.Open(volume, "missing.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
}

func TestValidateReconcilesImageIdentityChildBoundariesAndFamilyCycles(t *testing.T) {
	root := syntheticRoot(t)
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	_, diagnostics, err := Validate(t.Context(), ValidateInput{
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
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	_, diagnostics, err := Validate(t.Context(), ValidateInput{
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
