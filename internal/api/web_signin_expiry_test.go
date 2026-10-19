package api

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func TestWebSignInTabExpiryCancelsAuthority(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cfg := config.Default()
		cfg.Server.APIKey = "synthetic-key"
		cfg.Web.SessionLifetime = config.Duration(time.Minute)
		sessions := newWebSessionRegistry()
		sessions.login = newWebLoginPolicy(Deps{Cfg: cfg, WebLoginEnabled: true, WebURL: "http://localhost:7777/"})
		tab, err := sessions.issueLogin()
		require.NoError(t, err)
		_, ctx, ok := sessions.authenticate(tab.Token)
		require.True(t, ok)
		time.Sleep(time.Minute)
		synctest.Wait()
		_, _, ok = sessions.authenticate(tab.Token)
		require.False(t, ok)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		require.NoError(t, sessions.closeAll(t.Context()))
	})
}

func TestWebSessionCloseAllWaitsForInFlightRevocation(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseCallback()
	sessions := newWebSessionRegistry(func(string) {
		close(entered)
		<-release
	})
	token, _, err := sessions.issue()
	require.NoError(t, err)

	revokeDone := make(chan struct{})
	go func() {
		sessions.revoke(token)
		close(revokeDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("revocation callback did not start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- sessions.closeAll(context.Background()) }()
	require.Eventually(t, func() bool {
		sessions.mu.Lock()
		defer sessions.mu.Unlock()
		return sessions.closing
	}, time.Second, time.Millisecond)
	select {
	case err := <-closeDone:
		t.Fatalf("closeAll returned before owner cleanup completed: %v", err)
	default:
	}

	releaseCallback()
	select {
	case <-revokeDone:
	case <-time.After(time.Second):
		t.Fatal("revocation callback did not finish")
	}
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("closeAll did not finish after owner cleanup")
	}
}
