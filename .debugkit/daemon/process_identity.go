package daemon

import "os"

// ProcessIdentity is an opaque OS process-creation identity. Values are only
// meaningful for exact comparison against the live process holding a PID.
type ProcessIdentity string

// ProcessIdentityStatus describes whether a recorded identity can be matched.
type ProcessIdentityStatus uint8

const (
	ProcessIdentityUnknown ProcessIdentityStatus = iota
	ProcessIdentityMatch
	ProcessIdentityMismatch
)

// CompareProcessIdentity compares a recorded creation identity with the live
// process currently holding pid. Unknown never authorizes destructive action,
// and an identity from another PID namespace is Unknown unless it matches the
// live process exactly (see AGENTS.md).
func CompareProcessIdentity(pid int, recorded ProcessIdentity) ProcessIdentityStatus {
	return compareProcessIdentity(pid, recorded, ReadProcessIdentity, processIdentityScope)
}

func compareProcessIdentity(
	pid int,
	recorded ProcessIdentity,
	read func(int) (ProcessIdentity, bool),
	scope func(int, ProcessIdentity) ProcessScope,
) ProcessIdentityStatus {
	if recorded == "" {
		return ProcessIdentityUnknown
	}
	live, ok := read(pid)
	if ok && live == recorded {
		return ProcessIdentityMatch
	}
	if !ok {
		return ProcessIdentityUnknown
	}
	switch scope(pid, recorded) {
	case ProcessScopeLocal, ProcessScopeGone:
		return ProcessIdentityMismatch
	default:
		return ProcessIdentityUnknown
	}
}

// ProcessScope is what a caller can conclude from an identity alone about
// the process it names.
type ProcessScope uint8

const (
	// ProcessScopeUnknown means the identity or the caller's boot ID or namespace is unreadable.
	ProcessScopeUnknown ProcessScope = iota
	// ProcessScopeLocal means the PID names a process this caller can check.
	ProcessScopeLocal
	// ProcessScopeForeign means another PID namespace on this boot; its PID means nothing here.
	ProcessScopeForeign
	// ProcessScopeGone means the identity proves the process gone.
	ProcessScopeGone
)

// CompareRuntimeProcessIdentity compares the strongest process identity stored
// in rec with the live process. Versioned identities take precedence; legacy
// identities remain readable only on platforms where they are reliable.
func CompareRuntimeProcessIdentity(rec RuntimeRecord) ProcessIdentityStatus {
	return compareRuntimeProcessIdentity(rec, processIdentityScope)
}

// compareRuntimeProcessIdentity lets callers reuse a versioned identity's scope.
// Legacy identities still need their own compatibility check.
func compareRuntimeProcessIdentity(rec RuntimeRecord, scope func(int, ProcessIdentity) ProcessScope) ProcessIdentityStatus {
	if rec.ProcessIdentityV2 != "" {
		return compareProcessIdentity(rec.PID, rec.ProcessIdentityV2, ReadProcessIdentity, scope)
	}
	return CompareProcessIdentity(rec.PID, rec.ProcessIdentity)
}

// RuntimeProcessAlive reports whether rec may still have a live process: its
// versioned identity is one this caller can't judge, or it is local and the PID
// is alive here. An identity that proves the process gone ends the question
// whatever the local PID holds (see AGENTS.md). Use it in place of
// ProcessAlive(rec.PID) wherever a record is skipped or deleted.
func RuntimeProcessAlive(rec RuntimeRecord) bool {
	switch RuntimeProcessScope(rec) {
	case ProcessScopeLocal:
		return ProcessAlive(rec.PID)
	case ProcessScopeGone:
		return false
	default:
		return true
	}
}

// RuntimeProcessScope classifies the process named by rec's versioned identity.
// A record with no versioned identity is ProcessScopeLocal.
func RuntimeProcessScope(rec RuntimeRecord) ProcessScope {
	if rec.ProcessIdentityV2 == "" {
		return ProcessScopeLocal
	}
	return processIdentityScope(rec.PID, rec.ProcessIdentityV2)
}

// RuntimeRecordBlocksStart reports whether rec names a live PID in this namespace,
// or one kit can't rule out, whose identity isn't a mismatch. It never blocks for
// the caller's own PID. Callers still hold the start lock and probe the endpoint.
func RuntimeRecordBlocksStart(rec RuntimeRecord) bool {
	if rec.PID == os.Getpid() || !ProcessAlive(rec.PID) {
		return false
	}
	return runtimeRecordBlocksStart(rec, RuntimeProcessScope(rec))
}

func runtimeRecordBlocksStart(rec RuntimeRecord, scope ProcessScope) bool {
	switch scope {
	case ProcessScopeLocal:
		return compareRuntimeProcessIdentity(rec, func(int, ProcessIdentity) ProcessScope {
			return scope
		}) != ProcessIdentityMismatch
	case ProcessScopeUnknown:
		return true
	default:
		return false
	}
}

// runtimeRecordLive is RuntimeProcessAlive without a definite identity
// mismatch. Discover uses it to pick records worth probing.
func runtimeRecordLive(rec RuntimeRecord) bool {
	return RuntimeProcessAlive(rec) && CompareRuntimeProcessIdentity(rec) != ProcessIdentityMismatch
}
