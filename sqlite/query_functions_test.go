package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
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

	for _, tc := range []struct{ kind, desc, path, expected string }{
		{"cli", "", "/shots/a.jpg", "97310aecd6d353ecc4b0b3bbe2a48333e4be26fa435ef8edbce032e1da9fd52d"},
		{"cli", "", `C:\shots\a.jpg`, "18e6828f06efb295809a602b37c11176daeb34f7d3265b6cc6ce16452d69045c"},
		{"cli", "", `\\server\share\shots\a.jpg`, "91e3194e643686be077ed2f491b312794e390dab130e3503cec02778b6d2590e"},
		{"watch", "camera", "/shots/a.jpg", "24bdb20254864c349da512700a1fc77a465d1b042b60769fc3d187699ab9fad7"},
		{"push", "camera", "/shots/a.jpg", "043d20f8124ffecbfe429e8d5bc753d4525262686a362ba160497eb228cb5e66"},
		{"cli", "", "", ""},
	} {
		var key sql.NullString
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT docbank_photo_folder_v1(?, ?, ?)`, tc.kind, tc.desc, tc.path).Scan(&key))
		require.Equal(t, tc.expected, key.String)
		require.Equal(t, tc.expected != "", key.Valid)
		direct, valid := query.PhotoFolderKey(tc.kind, tc.desc, tc.path)
		require.Equal(t, direct, key.String)
		require.Equal(t, valid, key.Valid)
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
