package daemonconn_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestWatchChanges(t *testing.T) {
	t.Parallel()
	c, s := newClient(t, serverKey)
	ctx := t.Context()
	first, err := s.Mkdir(ctx, s.RootID(), "first")
	require.NoError(t, err)
	second, err := s.Mkdir(ctx, s.RootID(), "second")
	require.NoError(t, err)
	var events []store.VaultChange
	require.NoError(t, c.WatchChanges(ctx, "", false, func(event store.VaultChange) error { events = append(events, event); return nil }))
	require.Len(t, events, 2)
	require.Equal(t, first.ID, events[0].NodeID)
	require.Equal(t, second.ID, events[1].NodeID)
	require.Equal(t, "created", events[1].Kind)
	require.Equal(t, int64(1), events[1].Revision)
	var resumed []store.VaultChange
	require.NoError(t, c.WatchChanges(ctx, events[0].Cursor, false, func(event store.VaultChange) error { resumed = append(resumed, event); return nil }))
	require.Equal(t, events[1:], resumed)
	aborted := errors.New("stop reading")
	require.ErrorIs(t, c.WatchChanges(ctx, "", true, func(store.VaultChange) error { return aborted }), aborted)
	followCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, c.WatchChanges(followCtx, events[1].Cursor, true, nil), context.Canceled)
}

func TestWatchChangesExpired(t *testing.T) {
	t.Parallel()
	c, s := newClient(t, serverKey)
	_, head, err := s.VaultChanges(t.Context(), "", 500)
	require.NoError(t, err)
	foreign := base64.RawURLEncoding.EncodeToString([]byte("11111111-1111-4111-8111-111111111111:0"))
	called := false
	err = c.WatchChanges(t.Context(), foreign, false, func(store.VaultChange) error { called = true; return nil })
	expired, ok := errors.AsType[*daemonconn.ChangeCursorExpiredError](err)
	require.True(t, ok)
	require.Equal(t, head, expired.Cursor)
	require.ErrorIs(t, err, store.ErrChangeCursorExpired)
	require.False(t, called)
	require.Contains(t, err.Error(), "rescan, then resume with --after "+head)
}
