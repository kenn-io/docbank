package sqlite_test

import (
	"database/sql"
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

	exercisePhotoCaptureFunction(t, db)

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

func exercisePhotoCaptureFunction(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tc := range []struct{ value, precision, zone, offset, key string }{
		{"", "", "", "", ""}, {"2024-01-02T03:04:05+02:30", "second", "offset", "+02:30", "2024-01-02T00:34:05.000000000"}, {"2024-01-02T03:04:05.1234567891Z", "fraction", "utc", "", "2024-01-02T03:04:05.123456789"}, {"0000-01-01", "date", "omitted", "", ""},
	} {
		var key string
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT docbank_query_capture_time_v1(?,?,?,?)`, tc.value, tc.precision, tc.zone, tc.offset).Scan(&key))
		require.Equal(t, tc.key, key)
	}
	var key string
	require.Error(t, db.QueryRowContext(t.Context(), `SELECT docbank_query_capture_time_v1(NULL,'','','')`).Scan(&key))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT docbank_query_capture_time_v1(CAST('2024-01-02' AS BLOB),'date','omitted','')`).Scan(&key))
	require.Equal(t, "2024-01-02T00:00:00.000000000", key)
	require.Error(t, db.QueryRowContext(t.Context(), `SELECT docbank_query_capture_time_v1('bad','date','omitted','')`).Scan(&key))
}

func TestPhotoCaptureTimeFunction(t *testing.T) {
	for _, name := range sql.Drivers() {
		if name != "sqlite" && name != "docbank-sqlite3-query-v1" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			db, err := sql.Open(name, filepath.Join(t.TempDir(), "capture.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			exercisePhotoCaptureFunction(t, db)
		})
	}
}
