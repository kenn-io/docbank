package api

import (
	"context"
	"sync"
	"sync/atomic"
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

func TestConcurrentWebSessionRevokesReportOnce(t *testing.T) {
	t.Parallel()
	sessions := newWebSessionRegistry()
	token, _, err := sessions.issue()
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var calls atomic.Int32
	report := func() {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first := make(chan struct{})
	go func() {
		sessions.reportAndRevoke(token, report)
		close(first)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first revoke did not reach the report callback")
	}
	_, _, ok := sessions.authenticate(token)
	require.True(t, ok, "session remains live during capture")
	second := make(chan struct{})
	go func() {
		sessions.reportAndRevoke(token, report)
		close(second)
	}()
	select {
	case <-second:
	case <-ctx.Done():
		t.Fatal("duplicate revoke blocked on the report callback")
	}
	require.EqualValues(t, 1, calls.Load())
	_, _, ok = sessions.authenticate(token)
	require.True(t, ok, "duplicate revoke leaves the in-flight session live")
	unblock()
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("first revoke did not finish after capture")
	}
	_, _, ok = sessions.authenticate(token)
	require.False(t, ok)
	require.EqualValues(t, 1, calls.Load())
}
