//go:build linux

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const linuxProcessIdentityPrefix = "linux-v1:"

// ReadProcessIdentity returns a Linux process identity derived only from
// monotonic kernel state. It remains stable when the wall clock changes.
func ReadProcessIdentity(pid int) (ProcessIdentity, bool) {
	return readLinuxProcessIdentity("/proc", pid)
}

// processIdentityScope judges identity, recorded for pid, for the caller's own
// /proc.
func processIdentityScope(pid int, identity ProcessIdentity) ProcessScope {
	return linuxProcessIdentityScope("/proc", pid, identity)
}

// linuxProcessIdentityScope is ProcessScopeUnknown when identity is malformed or
// procRoot's boot ID or self namespace is unreadable. Otherwise it is
// ProcessScopeGone for an identity from an earlier boot or a provably gone nested
// process, ProcessScopeLocal for procRoot's own namespace, and ProcessScopeForeign for any
// other. pid is the record's PID in the identity's namespace.
func linuxProcessIdentityScope(procRoot string, pid int, identity ProcessIdentity) ProcessScope {
	bootID, namespace, ticks, ok := parseLinuxProcessIdentity(identity)
	ownBootID, bootOK := readLinuxBootID(procRoot)
	own, err := readLinuxPIDNamespaceAt(procRoot, "self")
	switch {
	case !ok || !bootOK || err != nil:
		return ProcessScopeUnknown
	case bootID != ownBootID || linuxNestedProcessProvedGone(procRoot, own, namespace, pid, ticks):
		return ProcessScopeGone
	case namespace == own:
		return ProcessScopeLocal
	}
	return ProcessScopeForeign
}

// initPIDNamespace is the inode of the kernel's initial PID namespace.
const initPIDNamespace = "4026531836"

// linuxNestedProcessProvedGone is true when namespace differs from own and
// linuxNestedProcessGone holds. Only a caller in the initial PID namespace
// sees every nested process, so only it can prove one gone.
func linuxNestedProcessProvedGone(procRoot, own, namespace string, pid int, ticks string) bool {
	return namespace != own && own == initPIDNamespace && linuxNestedProcessGone(procRoot, pid, ticks)
}

// linuxNestedProcessGone is true when no process in a nested PID namespace
// has pid as its innermost PID and started at ticks. It reads only the status
// and stat files, which any user can read, so it sees processes of every uid.
// A process that exits during the walk doesn't count. Any other read failure,
// or a procRoot that hides pid 1 (hidepid), means the answer is unknown, so it
// is false.
func linuxNestedProcessGone(procRoot string, pid int, ticks string) bool {
	if _, err := os.Stat(filepath.Join(procRoot, "1")); err != nil {
		return false
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false
	}
	want := strconv.Itoa(pid)
	for _, entry := range entries {
		hostPID, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		status, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "status"))
		if linuxProcessExited(err) {
			continue
		}
		if err != nil {
			return false
		}
		innermost, nested, ok := parseLinuxInnermostNSpid(status)
		if !ok {
			return false
		}
		if !nested || innermost != want {
			continue
		}
		started, err := readLinuxProcessStartTicks(procRoot, hostPID)
		if linuxProcessExited(err) {
			continue
		}
		if err != nil || started == ticks {
			return false
		}
	}
	return true
}

// linuxProcessExited is true for the errors /proc returns once a process is
// gone.
func linuxProcessExited(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// parseLinuxInnermostNSpid returns the last NSpid entry of a status file,
// which is the process's PID in its own namespace, and whether the process is
// in a namespace nested below the reader's.
func parseLinuxInnermostNSpid(status []byte) (innermost string, nested, ok bool) {
	for line := range strings.Lines(string(status)) {
		rest, found := strings.CutPrefix(line, "NSpid:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return "", false, false
		}
		return fields[len(fields)-1], len(fields) > 1, true
	}
	return "", false, false
}

func readLinuxBootID(procRoot string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, "sys/kernel/random/boot_id"))
	bootID := strings.TrimSpace(string(raw))
	return bootID, err == nil && validLinuxBootID(bootID)
}

// parseLinuxProcessIdentity returns the boot ID, PID namespace, and start
// ticks of a well-formed linux-v1 identity.
func parseLinuxProcessIdentity(identity ProcessIdentity) (bootID, namespace, ticks string, ok bool) {
	encoded := string(identity)
	if !strings.HasPrefix(encoded, linuxProcessIdentityPrefix) {
		return "", "", "", false
	}
	fields := strings.Split(strings.TrimPrefix(encoded, linuxProcessIdentityPrefix), ":")
	if len(fields) != 3 || !validLinuxBootID(fields[0]) {
		return "", "", "", false
	}
	for _, field := range fields[1:] {
		if _, valid := parseCanonicalLinuxIdentityUint(field); !valid {
			return "", "", "", false
		}
	}
	return fields[0], fields[1], fields[2], true
}

func validLinuxBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if (character < '0' || character > '9') &&
				(character < 'a' || character > 'f') {
				return false
			}
		}
	}
	return true
}

func parseCanonicalLinuxIdentityUint(encoded string) (uint64, bool) {
	value, err := strconv.ParseUint(encoded, 10, 64)
	if err != nil || value == 0 || strconv.FormatUint(value, 10) != encoded {
		return 0, false
	}
	return value, true
}

func runtimeProcessIdentities(pid int) (ProcessIdentity, ProcessIdentity) {
	identity, _ := ReadProcessIdentity(pid)
	// The legacy field stays empty so clients predating process_identity_v2
	// treat this record as unverifiable instead of deleting it as mismatched.
	return "", identity
}

func readLinuxProcessIdentity(procRoot string, pid int) (ProcessIdentity, bool) {
	if pid <= 0 {
		return "", false
	}
	bootIdentity, ok := readLinuxBootID(procRoot)
	if !ok {
		return "", false
	}
	namespace, err := readLinuxPIDNamespace(procRoot, pid)
	if err != nil {
		return "", false
	}
	processTicks, err := readLinuxProcessStartTicks(procRoot, pid)
	if err != nil {
		return "", false
	}
	return ProcessIdentity(fmt.Sprintf(
		"%s%s:%s:%s",
		linuxProcessIdentityPrefix,
		bootIdentity,
		namespace,
		processTicks,
	)), true
}

func readLinuxPIDNamespace(procRoot string, pid int) (string, error) {
	return readLinuxPIDNamespaceAt(procRoot, strconv.Itoa(pid))
}

func readLinuxPIDNamespaceAt(procRoot, process string) (string, error) {
	target, err := os.Readlink(filepath.Join(procRoot, process, "ns/pid"))
	if err != nil {
		return "", err
	}
	const prefix = "pid:["
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "]") {
		return "", errors.New("linux PID namespace has an invalid link target")
	}
	inode := strings.TrimSuffix(strings.TrimPrefix(target, prefix), "]")
	value, ok := parseCanonicalLinuxIdentityUint(inode)
	if !ok {
		return "", errors.New("linux PID namespace has an invalid inode")
	}
	return strconv.FormatUint(value, 10), nil
}

func readLinuxProcessStartTicks(procRoot string, pid int) (string, error) {
	stat, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	return parseLinuxProcessStartTicks(stat)
}

func parseLinuxProcessStartTicks(stat []byte) (string, error) {
	closing := bytes.LastIndexByte(stat, ')')
	if closing < 0 || closing+1 >= len(stat) {
		return "", errors.New("linux process stat has no command terminator")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	const startTimeIndex = 19 // Field 22 after PID and command.
	if len(fields) <= startTimeIndex {
		return "", errors.New("linux process stat has no start time")
	}
	value, ok := parseCanonicalLinuxIdentityUint(fields[startTimeIndex])
	if !ok {
		return "", errors.New("linux process stat has an invalid start time")
	}
	return strconv.FormatUint(value, 10), nil
}
