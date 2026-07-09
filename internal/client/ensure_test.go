//go:build unix

package client

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/version"
)

func TestCreateTimeMatches(t *testing.T) {
	rec := NewRecord("127.0.0.1:1", "key", "tok")
	require.Equal(t, os.Getpid(), rec.PID)
	assert.True(t, createTimeMatches(rec), "own record must match")

	// Simulate PID reuse: same live PID, different recorded create time.
	rec.Metadata[metaCreateTime] = strconv.FormatInt(1, 10)
	assert.False(t, createTimeMatches(rec), "mismatched create_time must read as dead")

	// Records without the key (older daemons) match trivially.
	delete(rec.Metadata, metaCreateTime)
	assert.True(t, createTimeMatches(rec))
}

// A version-matched record without a published API key is a pre-key daemon:
// Ensure's discovery must reject it (so the replace path stops it) instead of
// returning a client that is either unauthenticated or doomed to 401s. The
// any-version discovery used by status/stop must keep accepting it, or the
// stale daemon could never be stopped.
func TestEnsureDiscoveryRejectsKeylessRecords(t *testing.T) {
	rec := NewRecord("127.0.0.1:1", "key", "tok")
	info := kitdaemon.PingInfo{Version: version.Version}

	require.True(t, discoverOptions(true).Accept(rec, info))

	delete(rec.Metadata, metaAPIKey)
	assert.False(t, discoverOptions(true).Accept(rec, info),
		"version-matched but keyless record must be replaced, not used")
	assert.True(t, discoverOptions(false).Accept(rec, info),
		"status/stop discovery must still see keyless daemons")
}

// Same-version development builds can still have incompatible HTTP behavior.
// A missing or mismatched protocol revision must therefore force replacement;
// status/stop discovery remains permissive so the old daemon can be stopped.
func TestEnsureDiscoveryRejectsProtocolMismatch(t *testing.T) {
	rec := NewRecord("127.0.0.1:1", "key", "tok")
	info := kitdaemon.PingInfo{Version: version.Version}

	require.True(t, discoverOptions(true).Accept(rec, info))

	delete(rec.Metadata, metaProtocolVersion)
	assert.False(t, discoverOptions(true).Accept(rec, info),
		"same-version record without a protocol revision must be replaced")
	assert.True(t, discoverOptions(false).Accept(rec, info),
		"status/stop discovery must still see incompatible daemons")

	rec.Metadata[metaProtocolVersion] = "0"
	assert.False(t, discoverOptions(true).Accept(rec, info),
		"same-version record with a mismatched protocol revision must be replaced")
}
