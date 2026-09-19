package api

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebDownloadSessionRevocationFencesTicketIssuance(t *testing.T) {
	downloads := newWebDownloadRegistry(t.TempDir())
	sessions := newWebSessionRegistry(downloads.revokeOwner)
	token, _, err := sessions.issue()
	require.NoError(t, err)
	owner, _, ok := sessions.authenticate(token)
	require.True(t, ok)

	sessions.revoke(token)
	called := false
	active, err := sessions.withActiveOwner(owner, func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	assert.False(t, active)
	assert.False(t, called)
}

func TestArchiveTicketExpiryReleasesLeaseWithoutRemovingArchive(t *testing.T) {
	downloads := newWebDownloadRegistry(t.TempDir())
	path := filepath.Join(t.TempDir(), "retained.zip")
	require.NoError(t, os.WriteFile(path, []byte("synthetic archive"), 0600))
	file, err := os.Open(path)
	require.NoError(t, err)
	released := 0
	token, err := downloads.issue(webDownloadTicket{path: path, owner: "owner", archiveFile: file, releaseArchive: func() { released++; _ = file.Close() }})
	require.NoError(t, err)
	require.False(t, downloads.cancel("other", token))
	downloads.expire(sha256.Sum256([]byte(token)))
	require.Equal(t, 1, released)
	_, ok := downloads.consume(token)
	require.False(t, ok)
	require.FileExists(t, path)
}

func TestWebDownloadConcurrentRevocationRemovesTicketIssuedBeforeRevokeWins(t *testing.T) {
	downloads := newWebDownloadRegistry(t.TempDir())
	sessions := newWebSessionRegistry(downloads.revokeOwner)
	token, _, err := sessions.issue()
	require.NoError(t, err)
	owner, _, ok := sessions.authenticate(token)
	require.True(t, ok)
	staged := filepath.Join(t.TempDir(), "staged")
	require.NoError(t, os.WriteFile(staged, []byte("verified"), 0o600))

	entered := make(chan struct{})
	release := make(chan struct{})
	issued := make(chan string, 1)
	go func() {
		active, issueErr := sessions.withActiveOwner(owner, func() error {
			close(entered)
			<-release
			ticket, err := downloads.issue(webDownloadTicket{path: staged, owner: owner})
			if err == nil {
				issued <- ticket
			}
			return err
		})
		if !active || issueErr != nil {
			issued <- ""
		}
	}()
	<-entered
	revoked := make(chan struct{})
	go func() {
		sessions.revoke(token)
		close(revoked)
	}()
	close(release)
	issuedToken := <-issued
	<-revoked
	require.NotEmpty(t, issuedToken)
	_, exists := downloads.consume(issuedToken)
	assert.False(t, exists)
	_, statErr := os.Stat(staged)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}
