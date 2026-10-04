package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
)

// Removing registration on either adapter, or duplicating a less strict MIME
// classifier in SQL, breaks these consumer-visible query predicate results.
func exerciseQueryFunctions(t *testing.T, driver docsqlite.Driver) {
	t.Helper()
	db, err := driver.Open(filepath.Join(t.TempDir(), "query-functions.db"), docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Deferred,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	for _, test := range []struct{ mime, name, family string }{
		{"text/plain; charset=utf-8", "message.eml", "text"},
		{"message/rfc822", "message.txt", "email"},
		{"application/octet-stream", "REPORT.PDF", "document"},
		{"", "archive.zip", "archive"},
		{"image/synthetic", "opaque.bin", "image"},
		{"video/synthetic", "opaque.bin", "audio_video"},
		{"application/synthetic", "report.pdf", "unknown"},
		{"text/plain; charset=a; charset=b", "report.pdf", "unknown"},
		{"text/plain; charset=\"unterminated", "report.pdf", "unknown"},
	} {
		var family string
		require.NoError(t, db.QueryRowContext(t.Context(),
			`SELECT docbank_query_media_family_v1(?, ?)`, test.mime, test.name).Scan(&family))
		require.Equal(t, test.family, family, "%q / %q", test.mime, test.name)
	}

	// Force a second physical connection: the predicate must not depend on the
	// first connection's local registration or pool reuse.
	first, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, first.Close()) }()
	second, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, second.Close()) }()
	var family string
	require.NoError(t, second.QueryRowContext(t.Context(),
		`SELECT docbank_query_media_family_v1('application/pdf', 'report.txt')`).Scan(&family))
	require.Equal(t, "document", family)
}
