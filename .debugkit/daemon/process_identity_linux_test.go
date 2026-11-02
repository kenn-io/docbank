//go:build linux

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadLinuxProcessIdentityUsesInspectableTargetNamespace(t *testing.T) {
	proc := linuxProcFixture(t, fixtureBootID, "4026531836", "4026532448")

	identity, ok := readLinuxProcessIdentity(proc, 42)
	require.True(t, ok)
	assert.Equal(t, ProcessIdentity("linux-v1:"+fixtureBootID+":4026532448:202"), identity)
}

func TestReadLinuxProcessIdentityRejectsMalformedTargetNamespace(t *testing.T) {
	proc := linuxProcFixture(t, fixtureBootID, "4026531836", "4026532448")
	link := filepath.Join(proc, "42/ns/pid")
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink("pid:not-an-inode", link))

	_, ok := readLinuxProcessIdentity(proc, 42)
	assert.False(t, ok)
}

func TestLinuxProcessIdentityCompatibilityRejectsMalformedValues(t *testing.T) {
	tests := []ProcessIdentity{
		"linux-v1:",
		"linux-v1:not-a-boot-id:4026532448:202",
		"linux-v1:B08745A1-625B-4F8B-8AB9-0123456789AB:4026532448:202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:missing:202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:+4026532448:202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:04026532448:202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:+202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:0202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:0",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:202:extra",
	}
	for _, identity := range tests {
		assert.Equal(t, ProcessScopeUnknown, processIdentityScope(os.Getpid(), identity), string(identity))
		assert.Equal(t, ProcessIdentityUnknown, CompareProcessIdentity(os.Getpid(), identity), string(identity))
	}
}

func TestParseLinuxProcessStartTicksRejectsMalformedStat(t *testing.T) {
	_, err := parseLinuxProcessStartTicks([]byte("42 malformed"))
	require.Error(t, err)
}

const (
	fixtureBootID      = "b08745a1-625b-4f8b-8ab9-0123456789ab"
	fixtureOtherBootID = "11111111-2222-3333-4444-555555555555"
)

// linuxProcFixture builds a proc root whose self namespace is ownNamespace,
// with host pid 1 and pid 42, which runs in pidNamespace as pid 7 there. An
// empty bootID or ownNamespace leaves that file unreadable.
func linuxProcFixture(t *testing.T, bootID, ownNamespace, pidNamespace string) string {
	t.Helper()
	proc := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "sys/kernel/random"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "self/ns"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "42/ns"), 0o755))
	if bootID != "" {
		require.NoError(t, os.WriteFile(
			filepath.Join(proc, "sys/kernel/random/boot_id"),
			[]byte(bootID+"\n"),
			0o644,
		))
	}
	if ownNamespace != "" {
		require.NoError(t, os.Symlink("pid:["+ownNamespace+"]", filepath.Join(proc, "self/ns/pid")))
	}
	writeLinuxProcess(t, proc, 1, "1", "1")
	writeLinuxProcess(t, proc, 42, "42\t7", "202")
	require.NoError(t, os.Symlink("pid:["+pidNamespace+"]", filepath.Join(proc, "42/ns/pid")))
	return proc
}

// writeLinuxProcess writes the status and stat files of hostPID under proc.
func writeLinuxProcess(t *testing.T, proc string, hostPID int, nspid, startTicks string) {
	t.Helper()
	dir := filepath.Join(proc, strconv.Itoa(hostPID))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "status"),
		[]byte("Name:\tkwt\nNSpid:\t"+nspid+"\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "stat"),
		[]byte(linuxStatFixture(hostPID, "kwt daemon (worker)", startTicks)),
		0o644,
	))
}

func TestLinuxProcessIdentityScope(t *testing.T) {
	const (
		initNamespace  = "4026531836"
		childNamespace = "4026532448"
	)
	identity := func(bootID, namespace, ticks string) ProcessIdentity {
		return ProcessIdentity("linux-v1:" + bootID + ":" + namespace + ":" + ticks)
	}
	server := identity(fixtureBootID, childNamespace, "202")
	tests := []struct {
		name         string
		bootID       string // caller's boot ID; empty leaves it unreadable
		ownNamespace string // caller's PID namespace inode; empty leaves it unreadable
		pid          int    // record PID in the identity's namespace
		identity     ProcessIdentity
		tamper       func(t *testing.T, proc string)
		want         ProcessScope
	}{
		{"own namespace", fixtureBootID, childNamespace, 7, server, nil, ProcessScopeLocal},
		{"other namespace", fixtureBootID, childNamespace, 7, identity(fixtureBootID, initNamespace, "202"), nil, ProcessScopeForeign},
		{"other boot, own namespace", fixtureBootID, childNamespace, 7, identity(fixtureOtherBootID, childNamespace, "202"), nil, ProcessScopeGone},
		{"other boot, other namespace", fixtureBootID, childNamespace, 7, identity(fixtureOtherBootID, initNamespace, "202"), nil, ProcessScopeGone},
		{"unreadable own namespace", fixtureBootID, "", 7, server, nil, ProcessScopeUnknown},
		{"unreadable own boot ID", "", childNamespace, 7, server, nil, ProcessScopeUnknown},
		{"process present", fixtureBootID, initNamespace, 7, server, nil, ProcessScopeForeign},
		{"PID reused", fixtureBootID, initNamespace, 7, identity(fixtureBootID, childNamespace, "199"), nil, ProcessScopeGone},
		{"process absent", fixtureBootID, initNamespace, 8, server, nil, ProcessScopeGone},
		{"host process with the same PID and start", fixtureBootID, initNamespace, 43, server, func(t *testing.T, proc string) {
			t.Helper()
			writeLinuxProcess(t, proc, 43, "43", "202")
		}, ProcessScopeGone},
		{"unreadable status", fixtureBootID, initNamespace, 8, server, func(t *testing.T, proc string) {
			t.Helper()
			if os.Geteuid() == 0 {
				t.Skip("root can read any file")
			}
			writeLinuxProcess(t, proc, 43, "43\t9", "300")
			status := filepath.Join(proc, "43/status")
			require.NoError(t, os.Chmod(status, 0))
		}, ProcessScopeForeign},
		{"status without NSpid", fixtureBootID, initNamespace, 8, server, func(t *testing.T, proc string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(proc, "1/status"), []byte("Name:\tinit\n"), 0o644))
		}, ProcessScopeForeign},
		{"process exited mid-walk", fixtureBootID, initNamespace, 8, server, func(t *testing.T, proc string) {
			t.Helper()
			// A process directory without status, as when it exits during the walk.
			require.NoError(t, os.MkdirAll(filepath.Join(proc, "43"), 0o755))
		}, ProcessScopeGone},
		{"other users' processes hidden", fixtureBootID, initNamespace, 8, server, func(t *testing.T, proc string) {
			t.Helper()
			// hidepid=invisible hides pid 1 from everyone but root.
			require.NoError(t, os.RemoveAll(filepath.Join(proc, "1")))
		}, ProcessScopeForeign},
		{"caller not in the initial namespace", fixtureBootID, "4026532450", 8, server, nil, ProcessScopeForeign},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := linuxProcFixture(t, tt.bootID, tt.ownNamespace, childNamespace)
			if tt.tamper != nil {
				tt.tamper(t, proc)
			}
			assert.Equal(t, tt.want, linuxProcessIdentityScope(proc, tt.pid, tt.identity))
		})
	}
}

func TestCompareProcessIdentityMatchesBeforeJudgingNamespace(t *testing.T) {
	// The caller sees pid 42 in a child namespace but isn't in the initial
	// namespace, so it can't judge that namespace.
	proc := linuxProcFixture(t, fixtureBootID, "4026532450", "4026532448")
	read := func(pid int) (ProcessIdentity, bool) { return readLinuxProcessIdentity(proc, pid) }
	scope := func(pid int, identity ProcessIdentity) ProcessScope {
		return linuxProcessIdentityScope(proc, pid, identity)
	}
	live, ok := read(42)
	require.True(t, ok)
	require.Equal(t, ProcessScopeForeign, scope(42, live))

	assert.Equal(t, ProcessIdentityMatch, compareProcessIdentity(42, live, read, scope))

	mismatched := ProcessIdentity("linux-v1:" + fixtureBootID + ":4026532448:203")
	assert.Equal(t, ProcessIdentityUnknown, compareProcessIdentity(42, mismatched, read, scope))

	earlierBoot := ProcessIdentity("linux-v1:" + fixtureOtherBootID + ":4026532448:202")
	assert.Equal(t, ProcessIdentityMismatch, compareProcessIdentity(42, earlierBoot, read, scope))
}

func linuxStatFixture(pid int, command, startTicks string) string {
	return fmt.Sprintf(
		"%d (%s) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 %s 20",
		pid,
		command,
		startTicks,
	)
}
