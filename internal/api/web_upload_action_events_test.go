package api_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

// Drop a receipt at the real socket, after the daemon commits.
type lostReceiptWriter struct {
	http.ResponseWriter

	dropAfter int
}

func (w lostReceiptWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("hijack test socket: %w", err)
	}
	drop := &lostReceiptConn{Conn: conn, dropAfter: w.dropAfter}
	return drop, bufio.NewReadWriter(rw.Reader, bufio.NewWriter(drop)), nil
}

type lostReceiptConn struct {
	net.Conn

	receipts  int
	dropAfter int
}

func (c *lostReceiptConn) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte(`"type":"receipt"`)) || bytes.Contains(data, []byte(`"type":"photo_group_receipt"`)) {
		c.receipts++
		if c.receipts == c.dropAfter {
			_ = c.Close()
			return 0, io.ErrClosedPipe
		}
	}
	n, err := c.Conn.Write(data)
	if err != nil {
		return n, fmt.Errorf("write test socket: %w", err)
	}
	return n, nil
}

func TestBrowserImportsReportCommittedAdditionsAcrossRollbackAndLostReceipts(t *testing.T) {
	for _, photos := range []bool{false, true} {
		for _, dropAfter := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("photos=%t/drop=%d", photos, dropAfter), func(t *testing.T) {
				t.Setenv("DOCBANK_TELEMETRY_ENABLED", "1")
				t.Setenv("TELEMETRY_ENABLED", "1")
				capture := newActionCollector(t)
				ts, catalog := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = capture.capture })
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ts.Config.Handler.ServeHTTP(lostReceiptWriter{w, dropAfter}, r)
				}))
				defer proxy.Close()
				proxy.Client().Transport = ts.Client().Transport
				conn := browserUploadSocket(t, proxy)
				defer func() { _ = conn.CloseNow() }()
				data := []byte("synthetic data 0")
				importID := "synthetic-import"
				if photos {
					importID = planBrowserPhotos(t, conn, []map[string]any{
						{"relative_path": "file-0.JPG", "expected_size": len(data)},
						{"relative_path": "file-1.JPG", "expected_size": len(data)},
					})
					if dropAfter == 0 {
						db, err := store.DefaultSQLiteDriver().Open(catalog.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
						require.NoError(t, err)
						defer func() { require.NoError(t, db.Close()) }()
						// Defer the failure until Commit, after the group has produced an Added result.
						_, err = db.Exec(`CREATE TABLE synthetic_photo_failure(node_id INTEGER REFERENCES nodes(id) DEFERRABLE INITIALLY DEFERRED)`)
						require.NoError(t, err)
						_, err = db.Exec(`CREATE TRIGGER synthetic_photo_rollback AFTER INSERT ON photo_assets BEGIN INSERT INTO synthetic_photo_failure VALUES(-1); END`)
						require.NoError(t, err)
					}
				}
				count := max(1, dropAfter)
				for i := range count {
					data = []byte(fmt.Sprintf("synthetic data %d", i))
					hash := sha256.Sum256(data)
					expectedHash := hex.EncodeToString(hash[:])
					if dropAfter == 0 && !photos {
						expectedHash = strings.Repeat("a", 64)
					}
					begin := map[string]any{"type": "begin", "request_id": "file", "import_id": importID,
						"parent_id": catalog.RootID(), "name": fmt.Sprintf("file-%d.txt", i), "mime_type": "text/plain",
						"expected_hash": expectedHash, "expected_size": len(data)}
					if photos {
						begin = map[string]any{"type": "begin_photo_group", "request_id": "file", "import_id": importID, "group": i, "hashes": []string{expectedHash}}
					}
					require.NoError(t, wsjson.Write(t.Context(), conn, begin))
					var message map[string]any
					require.NoError(t, wsjson.Read(t.Context(), conn, &message))
					require.Equal(t, "ready", message["type"])
					require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, data))
					require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "file"}))
					err := wsjson.Read(t.Context(), conn, &message)
					if dropAfter == 0 {
						require.NoError(t, err)
						require.Equal(t, "error", message["type"])
					} else if i < dropAfter-1 {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
					}
				}
				for i := range count {
					path := fmt.Sprintf("/file-%d.txt", i)
					if photos {
						path = fmt.Sprintf("/photos/file-%d.JPG", i)
					}
					_, err := catalog.NodeByPath(t.Context(), path)
					if dropAfter == 0 {
						require.ErrorIs(t, err, store.ErrNotFound)
					} else {
						require.NoError(t, err)
					}
				}
				if dropAfter == 0 {
					require.NoError(t, capture.reporter.Close())
					require.Empty(t, capture.events)
				} else {
					capture.requireEvent(t, "document_added", map[string]any{"surface": "web"})
				}
			})
		}
	}
}
