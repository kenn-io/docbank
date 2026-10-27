package daemonconn

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/docbank/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestEnsureDiscoveryRejectsDaemonWithoutMissLogExport(t *testing.T) {
	t.Parallel()
	rec := NewRecord("127.0.0.1:1", "key", "tok", "")
	rec.Metadata[metaProtocolVersion] = "96" // Historical daemon lacks labeled-export routes.
	info := kitdaemon.PingInfo{Version: version.Version}
	assert.False(t, discoverOptions(true).Accept(rec, info), "new CLI must replace same-version daemon without labeled-export routes")
	assert.True(t, discoverOptions(false).Accept(rec, info), "status/stop must still discover the old daemon")
}
