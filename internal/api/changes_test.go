package api_test

import (
	"bufio"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/sqlite"
)

func TestVaultChangesDisconnectResume(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ts, s := newBubbleTestServer(t, nil)
		ctx := t.Context()
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
	})
}

func TestVaultChangesProblems(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	for _, query := range []string{"after=broken", "follow=invalid", "after=%ZZ", "limit=-1", "limit=501", "limit=bad", "limit=0&follow=true", "limit=0&after=broken"} {
		resp, _ := get(t, ts, "/api/v1/changes?"+query, nil)
		require.Equal(t, 400, resp.StatusCode, query)
	}
}

func TestVaultChangesEmptyFollowShutdown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ts, s := newBubbleTestServer(t, nil)
		ctx := t.Context()
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
	})
}

func TestVaultChangesLaterPollFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ts, s := newBubbleTestServer(t, nil)
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
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/changes?follow=true&after="+url.QueryEscape(cursor), nil)
		require.NoError(t, err)
		response, err := ts.Client().Do(req)
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
	})
}

func TestVaultChangesUnreadClientShutdown(t *testing.T) {
	t.Parallel()
	_, s := newTestServer(t, nil)
	_, err := s.CreateFile(t.Context(), s.RootID(), "busy.txt", testHash("busy"), 4, "text/plain")
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, testWebURL+"api/v1/changes", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		request.Header.Set("X-Api-Key", testAPIKey)
		response := newStalledResponse()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.Server.Handler().ServeHTTP(response, request)
		}()
		synctest.Wait()
		require.True(t, response.stalled(), "the stream should block writing to a client that stopped reading")
		s.Server.Close()
		<-done
	})
}

// stalledResponse models a client that stopped reading. Every body write
// blocks until the server sets a write deadline, as a full socket does.
type stalledResponse struct {
	header   http.Header
	mu       sync.Mutex
	writing  bool
	deadline chan struct{}
}

func newStalledResponse() *stalledResponse {
	return &stalledResponse{header: http.Header{}, deadline: make(chan struct{})}
}

func (r *stalledResponse) Header() http.Header { return r.header }

func (r *stalledResponse) WriteHeader(int) {}

func (r *stalledResponse) Write([]byte) (int, error) {
	r.mu.Lock()
	r.writing = true
	r.mu.Unlock()
	<-r.deadline
	return 0, os.ErrDeadlineExceeded
}

func (r *stalledResponse) FlushError() error { return nil }

func (r *stalledResponse) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		r.mu.Lock()
		defer r.mu.Unlock()
		select {
		case <-r.deadline:
		default:
			close(r.deadline)
		}
	}
	return nil
}

func (r *stalledResponse) stalled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writing
}

func TestVaultChangesCurrentCheckpoint(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	var ids []int64
	for i := range 501 {
		node, err := s.Mkdir(t.Context(), s.RootID(), fmt.Sprintf("existing-%d", i))
		require.NoError(t, err)
		ids = append(ids, node.ID)
	}
	response, body := get(t, ts, "/api/v1/changes?limit=0", nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var checkpoint api.ChangeEvent
	require.NoError(t, json.Unmarshal([]byte(body), &checkpoint))
	require.Equal(t, "result", checkpoint.Type)
	require.NotEmpty(t, checkpoint.Cursor)
	require.Nil(t, checkpoint.Change)
	// A bounded page returns the snapshot head, including unread events.
	_, body = get(t, ts, "/api/v1/changes?limit=2", nil)
	records := strings.Split(strings.TrimSpace(body), "\n")
	require.Len(t, records, 3)
	var event, terminal api.ChangeEvent
	require.NoError(t, json.Unmarshal([]byte(records[1]), &event))
	require.NotNil(t, event.Change)
	require.Equal(t, ids[1], event.Change.NodeID)
	require.NoError(t, json.Unmarshal([]byte(records[2]), &terminal))
	require.Equal(t, "result", terminal.Type)
	require.Equal(t, checkpoint.Cursor, terminal.Cursor)
	// Omitting the limit still drains all retained history.
	_, body = get(t, ts, "/api/v1/changes", nil)
	records = strings.Split(strings.TrimSpace(body), "\n")
	require.Len(t, records, 502)
	require.NoError(t, json.Unmarshal([]byte(records[500]), &event))
	require.Equal(t, ids[500], event.Change.NodeID)
	require.NoError(t, json.Unmarshal([]byte(records[501]), &terminal))
	require.Equal(t, "result", terminal.Type)
	require.Equal(t, checkpoint.Cursor, terminal.Cursor)
	// A full finite page ends even though more retained events remain.
	_, body = get(t, ts, "/api/v1/changes?limit=500", nil)
	records = strings.Split(strings.TrimSpace(body), "\n")
	require.Len(t, records, 501)
	require.NoError(t, json.Unmarshal([]byte(records[499]), &event))
	require.Equal(t, ids[499], event.Change.NodeID)
	require.NoError(t, json.Unmarshal([]byte(records[500]), &terminal))
	require.Equal(t, "result", terminal.Type)
	require.Equal(t, checkpoint.Cursor, terminal.Cursor)
	_, body = get(t, ts, "/api/v1/changes?limit=500&after="+url.QueryEscape(checkpoint.Cursor), nil)
	var repeated api.ChangeEvent
	require.NoError(t, json.Unmarshal([]byte(body), &repeated))
	require.Equal(t, "result", repeated.Type)
	require.Equal(t, checkpoint.Cursor, repeated.Cursor)
}
