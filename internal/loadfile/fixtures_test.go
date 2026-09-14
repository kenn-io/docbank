package loadfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func syntheticRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string][]byte{
		"VOL001/DATA/ab-package.dat":    []byte("DOCID\x14PARENT\nDOC-A\x14\nDOC-B\x14DOC-A\n"),
		"VOL001/DATA/ab-package.opt":    []byte("DOC-A,VOL001,IMAGES\\001\\DOC-A-1.tif,Y,,,2\nDOC-A,VOL001,IMAGES\\001\\DOC-A-2.tif,,,,\nDOC-B,VOL001,IMAGES\\001\\DOC-B-1.tif,Y,,,1\n"),
		"VOL001/IMAGES/001/DOC-A-1.tif": []byte("synthetic-tiff-a1"),
		"VOL001/IMAGES/001/DOC-A-2.tif": []byte("synthetic-tiff-a2"),
		"VOL001/IMAGES/001/DOC-B-1.tif": []byte("synthetic-tiff-b1"),
		"VOL001/TEXT/DOC-A.txt":         []byte("synthetic text A"),
		"VOL001/TEXT/DOC-B.txt":         []byte("synthetic text B"),
		"VOL001/NATIVES/DOC-A.pdf":      []byte("%PDF-1.7\nsynthetic A"),
	}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
	return root
}

func abPackageWithFaults(t *testing.T) ValidateInput {
	t.Helper()
	root := syntheticRoot(t)
	require.NoError(t, os.Remove(filepath.Join(root, "VOL001", "IMAGES", "001", "DOC-A-2.tif")))
	resolver, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	head, err := os.ReadFile(filepath.Join(root, "VOL001", "DATA", "ab-package.dat"))
	require.NoError(t, err)
	profile, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	return ValidateInput{
		Profile: profile,
		Records: []Record{
			{RowID: "row-a", DocID: "DOC-A", LoadFile: "ab-package.dat", RowOrdinal: 1,
				Files: []FileRef{{Role: "native", Volume: "VOL001", RelPath: "NATIVES/DOC-A.pdf", Status: "available"}}},
			{RowID: "row-a-duplicate", DocID: "DOC-A", LoadFile: "ab-package.dat", RowOrdinal: 2,
				Family: Family{ParentDocID: "DOC-MISSING"}},
		},
		Images: []ImageRef{
			{ImageKey: "DOC-A", Volume: "VOL001", RelPath: "IMAGES/001/DOC-A-1.tif", DocumentBreak: true, PageOrdinal: 1, DeclaredPageCount: 3},
			{ImageKey: "DOC-A", Volume: "VOL001", RelPath: "IMAGES/001/DOC-A-2.tif", PageOrdinal: 2},
		},
		Volumes:  []Volume{{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}},
		Resolver: resolver,
		Head:     head,
		PageCount: func(_ *os.File) (int, error) {
			return 2, nil
		},
	}
}

func diagnosticCodes(diagnostics []Diagnostic) []string {
	codes := make([]string, len(diagnostics))
	for i := range diagnostics {
		codes[i] = diagnostics[i].Code
	}
	return codes
}
