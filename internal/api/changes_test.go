package api_test

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/sqlite"
)

func TestVaultChangesDisconnectResume(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	node, err := s.CreateFile(ctx, s.RootID(), "first.txt", testHash("first"), 4, "text/plain")
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/changes?follow=true", nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 200, resp.StatusCode)
	scanner := bufio.NewScanner(resp.Body)
	require.True(t, scanner.Scan())
	var saved api.ChangeEvent
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &saved))
	require.Equal(t, node.ID, saved.Change.NodeID)
	require.Equal(t, "created", saved.Change.Kind)
	require.NoError(t, resp.Body.Close())
	var ids []int64
	for i := range 3 {
		n, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("new-%d.txt", i), testHash(strconv.Itoa(i)), 4, "text/plain")
		require.NoError(t, err)
		ids = append(ids, n.ID)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/changes?follow=true&after="+url.QueryEscape(saved.Change.Cursor), nil)
	require.NoError(t, err)
	resumed, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resumed.Body.Close() }()
	require.Equal(t, 200, resumed.StatusCode)
	scanner = bufio.NewScanner(resumed.Body)
	for _, id := range ids {
		require.True(t, scanner.Scan())
		var event api.ChangeEvent
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
		require.Equal(t, id, event.Change.NodeID)
		require.Equal(t, "created", event.Change.Kind)
	}
	// Follow continues to read writes made after the resumed page was drained.
	next, err := s.Mkdir(ctx, s.RootID(), "later")
	require.NoError(t, err)
	require.True(t, scanner.Scan())
	var event api.ChangeEvent
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
	require.Equal(t, next.ID, event.Change.NodeID)
	require.NoError(t, resumed.Body.Close())
}

func TestVaultChangesProblems(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	for _, query := range []string{"after=broken", "follow=invalid", "after=%ZZ"} {
		resp, _ := get(t, ts, "/api/v1/changes?"+query, nil)
		require.Equal(t, 400, resp.StatusCode, query)
	}
}

func TestVaultChangesEmptyFollowShutdown(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/changes?follow=true", nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// Shutdown ends an idle follow stream, including one with no events yet.
	s.Server.Close()
	scanner := bufio.NewScanner(resp.Body)
	require.False(t, scanner.Scan())
	require.NoError(t, scanner.Err())
}

func TestVaultChangesLaterPollFailure(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ts.Client().Timeout = 5 * time.Second
	db, err := s.SQLiteDriver().Open(s.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	node, err := s.CreateFile(t.Context(), s.RootID(), "processed.txt", testHash("processed"), 4, "text/plain")
	require.NoError(t, err)
	_, cursor, err := s.VaultChanges(t.Context(), "", 1)
	require.NoError(t, err)
	// Stage a real retention prefix before opening the stream, then commit it
	// after the response starts so a slow insert cannot race the first poll.
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO vault_changes WITH RECURSIVE n(i) AS (SELECT 2 UNION ALL SELECT i+1 FROM n WHERE i<100002) SELECT i,'processed',?,1 FROM n`, node.ID)
	require.NoError(t, err)
	_, err = tx.Exec(`DELETE FROM vault_changes WHERE seq<=2`)
	require.NoError(t, err)
	response, err := ts.Client().Get(ts.URL + "/api/v1/changes?follow=true&after=" + url.QueryEscape(cursor))
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, 200, response.StatusCode)
	require.NoError(t, tx.Commit())
	_, head, err := s.VaultChanges(t.Context(), "", 1)
	require.NoError(t, err)
	scanner := bufio.NewScanner(response.Body)
	require.True(t, scanner.Scan())
	var terminal api.ChangeEvent
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &terminal))
	require.Equal(t, "error", terminal.Type)
	require.NotNil(t, terminal.Error)
	require.Equal(t, "changes_cursor_expired", terminal.Error.Code)
	require.Equal(t, head, terminal.Error.Cursor)
}

func TestVaultChangesUnreadClientShutdown(t *testing.T) {
	t.Parallel()
	_, s := newTestServer(t, nil)
	db, err := s.SQLiteDriver().Open(s.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "busy.txt", testHash("busy"), 4, "text/plain")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO vault_changes WITH RECURSIVE n(i) AS (SELECT 2 UNION ALL SELECT i+1 FROM n WHERE i<100000) SELECT i,'processed',?,1 FROM n`, node.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		s.Server.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	addr, err := net.ResolveTCPAddr("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	conn, err := net.DialTCP("tcp", nil, addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetReadBuffer(1024))
	_, err = fmt.Fprintf(conn, "GET /api/v1/changes HTTP/1.1\r\nHost: %s\r\nX-Api-Key: %s\r\n\r\n", server.Listener.Addr(), testAPIKey)
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	// Leave the backlog unread. Shutdown must release the socket writer.
	select {
	case <-done:
		t.Fatal("backlog ended before shutdown")
	case <-time.After(250 * time.Millisecond): //nolint:kennlint // deadlinetest: Let the real TCP peer's unread receive window fill before shutdown.
	}
	s.Server.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("change stream remains blocked after daemon shutdown")
	}
}
