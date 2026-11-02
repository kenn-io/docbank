//go:build !linux

package daemon

import (
	"strconv"

	"github.com/shirou/gopsutil/v4/process"
)

// ReadProcessIdentity returns pid's OS-reported creation time as an opaque
// identity. ok is false when pid is gone or the platform cannot inspect it.
func ReadProcessIdentity(pid int) (ProcessIdentity, bool) {
	if pid <= 0 {
		return "", false
	}
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return "", false
	}
	created, err := proc.CreateTime()
	if err != nil || created <= 0 {
		return "", false
	}
	return ProcessIdentity(strconv.FormatInt(created, 10)), true
}

func processIdentityCompatible(identity ProcessIdentity) bool {
	encoded := string(identity)
	value, err := strconv.ParseInt(encoded, 10, 64)
	return err == nil && value > 0 && strconv.FormatInt(value, 10) == encoded
}

// processIdentityScope is ProcessScopeLocal for a readable identity whatever pid:
// non-Linux identities carry no boot ID or namespace.
func processIdentityScope(_ int, identity ProcessIdentity) ProcessScope {
	if !processIdentityCompatible(identity) {
		return ProcessScopeUnknown
	}
	return ProcessScopeLocal
}

func runtimeProcessIdentities(pid int) (ProcessIdentity, ProcessIdentity) {
	identity, _ := ReadProcessIdentity(pid)
	return identity, ""
}
