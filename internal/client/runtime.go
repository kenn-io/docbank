package client

import (
	"math"
	"strconv"

	"github.com/shirou/gopsutil/v4/process"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/version"
)

const (
	// Service is the daemon's runtime-record service name.
	Service = "docbank"
	// EnvBackgroundDaemon marks a process as an auto-spawned background
	// daemon (as opposed to a foreground `docbank serve`).
	EnvBackgroundDaemon = "DOCBANK_BACKGROUND_DAEMON"
	metaCreateTime      = "create_time"
	metaShutdownToken   = "shutdown_token"
)

// RuntimeStore returns the kit runtime-record store rooted at root.
func RuntimeStore(root string) kitdaemon.RuntimeStore {
	return kitdaemon.RuntimeStore{Dir: root, Prefix: "daemon"}
}

// NewRecord builds this process's runtime record. create_time guards the
// recorded PID against reuse: kit's record has no such field, so docbank
// carries it in Metadata (msgvault's pattern) and checks it before trusting
// or signaling a PID.
func NewRecord(addr, token string) kitdaemon.RuntimeRecord {
	rec := kitdaemon.NewRuntimeRecord(Service, version.Version,
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: addr})
	if rec.Metadata == nil {
		rec.Metadata = map[string]string{}
	}
	rec.Metadata[metaShutdownToken] = token
	if ct, ok := processCreateTimeMillis(rec.PID); ok {
		rec.Metadata[metaCreateTime] = strconv.FormatInt(ct, 10)
	}
	return rec
}

func processCreateTimeMillis(pid int) (int64, bool) {
	if pid <= 0 || pid > math.MaxInt32 {
		return 0, false
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, false
	}
	created, err := p.CreateTime()
	if err != nil {
		return 0, false
	}
	return created, true
}

// createTimeMatches reports whether rec's recorded create_time still
// describes the live process at rec.PID. A record without the key matches
// trivially (older daemons); a mismatch means PID reuse — treat as dead.
//
//nolint:unused // consumed by the client discovery task.
func createTimeMatches(rec kitdaemon.RuntimeRecord) bool {
	recorded := rec.Metadata[metaCreateTime]
	if recorded == "" {
		return true
	}
	live, ok := processCreateTimeMillis(rec.PID)
	if !ok {
		return false
	}
	return recorded == strconv.FormatInt(live, 10)
}
