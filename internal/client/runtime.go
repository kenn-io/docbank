package client

import (
	"math"
	"net"
	"strconv"

	"github.com/shirou/gopsutil/v4/process"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/version"
)

const (
	// Service is the daemon's runtime-record service name.
	Service = "docbank"
	// EnvBackgroundDaemon marks a process as an auto-spawned background
	// daemon (as opposed to a foreground `docbank daemon run`).
	EnvBackgroundDaemon = "DOCBANK_BACKGROUND_DAEMON"
	metaCreateTime      = "create_time"
	metaShutdownToken   = "shutdown_token"
	metaAPIKey          = "api_key"
	metaProtocolVersion = "protocol_version"
	metaWebAddress      = "web_address"
	// Bump whenever a newer CLI cannot safely use an older daemon's HTTP or
	// runtime-record contract, even when both binaries report the same version.
	daemonProtocolVersion = "41"
)

// EnsureResult reports what EnsureDaemon found or did.
type EnsureResult struct {
	// Record is the version- and protocol-matched daemon now running.
	Record kitdaemon.RuntimeRecord
	// Started is true when EnsureDaemon spawned a new daemon.
	Started bool
	// Replaced is the incompatible daemon EnsureDaemon stopped before
	// starting Record, nil when nothing was replaced.
	Replaced *kitdaemon.RuntimeRecord
}

// RuntimeStore returns the kit runtime-record store rooted at root.
func RuntimeStore(root string) kitdaemon.RuntimeStore {
	return kitdaemon.RuntimeStore{Dir: root, Prefix: "daemon"}
}

// NewRecord builds this process's runtime record. create_time guards the
// recorded PID against reuse: kit's record has no such field, so docbank
// carries it in Metadata (msgvault's pattern) and checks it before trusting
// or signaling a PID. apiKey is the daemon's effective API key (configured
// or freshly generated); publishing it here, inside owner-private DOCBANK_HOME,
// is how same-user CLI invocations authenticate without a separate secret
// channel — the same pattern the shutdown token already uses. The protocol
// revision prevents a same-version client from trusting an incompatible
// daemon. webAddress is the dedicated per-run browser listener; an empty value
// advertises that this binary cannot serve the compiled web application.
func NewRecord(addr, apiKey, token, webAddress string) kitdaemon.RuntimeRecord {
	rec := kitdaemon.NewRuntimeRecord(Service, version.Version,
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: addr})
	if rec.Metadata == nil {
		rec.Metadata = map[string]string{}
	}
	rec.Metadata[metaAPIKey] = apiKey
	rec.Metadata[metaShutdownToken] = token
	rec.Metadata[metaProtocolVersion] = daemonProtocolVersion
	if webAddress != "" {
		rec.Metadata[metaWebAddress] = webAddress
	}
	if ct, ok := processCreateTimeMillis(rec.PID); ok {
		rec.Metadata[metaCreateTime] = strconv.FormatInt(ct, 10)
	}
	return rec
}

func validWebAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
