package daemonconn_test

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestWatchChanges(t *testing.T) {
	t.Parallel()
	c, s := newClient(t, serverKey)
	ctx := t.Context()
	baseline, err := c.WatchChanges(ctx, "", false, nil)
	require.NoError(t, err)
	require.NotEmpty(t, baseline)
	_, err = s.Mkdir(ctx, s.RootID(), "first")
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "second")
	require.NoError(t, err)
	var events []store.VaultChange
	_, err = c.WatchChanges(ctx, baseline, false, func(event store.VaultChange) error { events = append(events, event); return nil })
	require.NoError(t, err)
	require.Len(t, events, 2)
	aborted := errors.New("stop reading")
	_, err = c.WatchChanges(ctx, "", true, func(store.VaultChange) error { return aborted })
	require.ErrorIs(t, err, aborted)
	foreign := base64.RawURLEncoding.EncodeToString([]byte("11111111-1111-4111-8111-111111111111:0"))
	_, err = c.WatchChanges(ctx, foreign, false, nil)
	expired, ok := errors.AsType[*daemonconn.ChangeCursorExpiredError](err)
	require.True(t, ok)
	require.Equal(t, events[1].Cursor, expired.Cursor)
	require.ErrorIs(t, err, store.ErrChangeCursorExpired)
}

func TestWatchChangesTerminalRecords(t *testing.T) {
	t.Parallel()
	cursor := base64.RawURLEncoding.EncodeToString([]byte("11111111-1111-4111-8111-111111111111:502"))
	for _, tc := range []struct {
		name   string
		body   string
		want   error
		detail string
	}{
		{"unexpected EOF", "", io.ErrUnexpectedEOF, "unexpected EOF"},
		{"expired cursor", `{"type":"error","error":{"code":"changes_cursor_expired","status":410,"cursor":"` + cursor + `"}}`, store.ErrChangeCursorExpired, "rescan, then resume with --after " + cursor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			_, err := daemonconn.New(server.URL, "key").WatchChanges(t.Context(), "", false, func(store.VaultChange) error { t.Fatal("control record delivered as change"); return nil })
			server.Close()
			require.ErrorContains(t, err, tc.detail)
			require.ErrorIs(t, err, tc.want)
		})
	}
}
