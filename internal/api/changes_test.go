package api_test

import (
	"bufio"
	"context"
	"encoding/base64"
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
	"go.kenn.io/docbank/internal/store"
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
	var saved store.VaultChange
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &saved))
	require.Equal(t, node.ID, saved.NodeID)
	require.Equal(t, "created", saved.Kind)
	require.NoError(t, resp.Body.Close())
	var ids []int64
	for i := range 3 {
		n, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("new-%d.txt", i), testHash(strconv.Itoa(i)), 4, "text/plain")
		require.NoError(t, err)
		ids = append(ids, n.ID)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/changes?follow=true&after="+url.QueryEscape(saved.Cursor), nil)
	require.NoError(t, err)
	resumed, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resumed.Body.Close() }()
	require.Equal(t, 200, resumed.StatusCode)
	scanner = bufio.NewScanner(resumed.Body)
	for _, id := range ids {
		require.True(t, scanner.Scan())
		var event store.VaultChange
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
		require.Equal(t, id, event.NodeID)
		require.Equal(t, "created", event.Kind)
		require.Equal(t, int64(1), event.Revision)
	}
	// Follow continues to read writes made after the resumed page was drained.
	next, err := s.Mkdir(ctx, s.RootID(), "later")
	require.NoError(t, err)
	require.True(t, scanner.Scan())
	var event store.VaultChange
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
	require.Equal(t, next.ID, event.NodeID)
	require.NoError(t, resumed.Body.Close())
	response, body := get(t, ts, "/api/v1/changes?after="+url.QueryEscape(event.Cursor), nil)
	require.Equal(t, 200, response.StatusCode)
	require.Empty(t, body)
}

func TestVaultChangesProblemsAndBrowserSession(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	_, head, err := s.VaultChanges(t.Context(), "", 500)
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(head)
	require.NoError(t, err)
	epoch, _, _ := strings.Cut(string(raw), ":")
	future := base64.RawURLEncoding.EncodeToString([]byte(epoch + ":1"))
	resp, body := get(t, ts, "/api/v1/changes?after="+future, nil)
	require.Equal(t, 410, resp.StatusCode)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "changes_cursor_expired", problem.Code)
	require.Equal(t, head, problem.Cursor)
	resp, _ = get(t, ts, "/api/v1/changes?after=broken", nil)
	require.Equal(t, 400, resp.StatusCode)
	resp, _ = get(t, ts, "/api/v1/changes?follow=invalid", nil)
	require.Equal(t, 400, resp.StatusCode)
	resp, body = do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, 201, resp.StatusCode)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	resp, body = get(t, ts, "/api/v1/changes", map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token})
	require.Equal(t, 403, resp.StatusCode)
	require.Contains(t, body, `"code":"web_session_read_only"`)
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
