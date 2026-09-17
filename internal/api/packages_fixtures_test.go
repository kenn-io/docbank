package api_test

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

type packageTableCounts struct {
	nodes, contentVersions, blobs, packagePreflights int
}

func tableCounts(t *testing.T, s *testStore) packageTableCounts {
	t.Helper()
	driver := store.DefaultSQLiteDriver()
	db, err := driver.Open(s.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var result packageTableCounts
	require.NoError(t, db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM nodes), (SELECT COUNT(*) FROM content_versions),
		(SELECT COUNT(*) FROM blobs), (SELECT COUNT(*) FROM package_preflights)`).Scan(
		&result.nodes, &result.contentVersions, &result.blobs, &result.packagePreflights))
	return result
}

func syntheticPackageRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	twoPagePDF, err := os.ReadFile(filepath.Join("..", "..", "document", "testdata", "scanassessment", "mixed-below-ratio.pdf"))
	require.NoError(t, err)
	onePagePDF, err := os.ReadFile(filepath.Join("..", "..", "document", "testdata", "scanassessment", "blank.pdf"))
	require.NoError(t, err)
	files := map[string][]byte{
		"VOL001/DATA/ab-package.dat":    []byte("þDOCIDþ\x14þNATIVEþ\r\nþDOC-Aþ\x14þNATIVES/DOC-A.pdfþ\r\nþDOC-Bþ\x14þNATIVES/DOC-B.pdfþ\r\n"),
		"VOL001/DATA/ab-package.opt":    []byte("DOC-A,VOL001,IMAGES\\001\\DOC-A-1.tif,Y,,,2\r\nDOC-A,VOL001,IMAGES\\001\\DOC-A-2.tif,,,,\r\nDOC-B,VOL001,IMAGES\\001\\DOC-B-1.tif,Y,,,1\r\n"),
		"VOL001/IMAGES/001/DOC-A-1.tif": []byte("synthetic-a1"),
		"VOL001/IMAGES/001/DOC-A-2.tif": []byte("synthetic-a2"),
		"VOL001/IMAGES/001/DOC-B-1.tif": []byte("synthetic-b1"),
		"VOL001/NATIVES/DOC-A.pdf":      twoPagePDF,
		"VOL001/NATIVES/DOC-B.pdf":      onePagePDF,
	}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
	return root
}

func blockingPreflightBody(t *testing.T) string {
	t.Helper()
	root := syntheticPackageRoot(t)
	require.NoError(t, os.Remove(filepath.Join(root, "VOL001", "IMAGES", "001", "DOC-A-2.tif")))
	body, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
	require.NoError(t, err)
	return string(body)
}
