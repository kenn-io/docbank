package api_test

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
		require.Equal(t, int64(1), event.Change.Revision)
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

func TestVaultChangesLaterPageFailure(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ts.Client().Timeout = 5 * time.Second
	db, err := s.SQLiteDriver().Open(s.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	// A full first page is readable; pruning overtakes the next page.
	_, err = db.Exec(`INSERT INTO vault_changes WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<502) SELECT i,'created',1,1 FROM n`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE vault_change_head SET high_water=502`)
	require.NoError(t, err)
	_, head, err := s.VaultChanges(t.Context(), "", 1)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM vault_changes WHERE seq=501`)
	require.NoError(t, err)
	response, body := get(t, ts, "/api/v1/changes", nil)
	require.Equal(t, 200, response.StatusCode)
	lines := strings.Split(strings.TrimSpace(body), "\n")
	require.Len(t, lines, 501)
	var terminal api.ChangeEvent
	require.NoError(t, json.Unmarshal([]byte(lines[500]), &terminal))
	require.Equal(t, "error", terminal.Type)
	require.NotNil(t, terminal.Error)
	require.Equal(t, "changes_cursor_expired", terminal.Error.Code)
	require.Equal(t, head, terminal.Error.Cursor)
}
