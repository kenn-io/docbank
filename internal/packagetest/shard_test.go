package packagetest

import (
	"bytes"
	"cmp"
	"errors"
	"flag"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	shardTestParentEnv = "DOCBANK_SHARD_TEST_PARENT"
	shardTestRecordEnv = "DOCBANK_SHARD_TEST_RECORD"
	shardTestFailEnv   = "DOCBANK_SHARD_TEST_FAIL"
	shardTestExitEnv   = "DOCBANK_SHARD_TEST_EXIT"
	shardTestMeetEnv   = "DOCBANK_SHARD_TEST_MEET"
)

// recordShardRun records which process ran the calling test, and the
// -test.parallel value it ran with, when a TestRunShards parent started this
// binary. TestShardChildA and
// TestShardChildB wait for each other, so they finish only when two children
// run at once.
func recordShardRun(t *testing.T) {
	t.Helper()
	if os.Getenv(shardTestParentEnv) != "1" {
		return
	}
	// A helper process the test starts from this binary must run as a test
	// binary, not wait for tests to serve.
	require.Empty(t, os.Getenv(shardMarkerEnv))
	name := strings.ReplaceAll(t.Name(), "/", ".") + "-" + strconv.Itoa(os.Getpid())
	parallel := flag.Lookup("test.parallel").Value.String()
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv(shardTestRecordEnv), name), []byte(parallel), 0o600))
	if t.Name() == "TestShardChildA" || t.Name() == "TestShardChildB" {
		connection, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", os.Getenv(shardTestMeetEnv))
		require.NoError(t, err)
		_, err = connection.Read(make([]byte, 1))
		require.NoError(t, errors.Join(err, connection.Close()))
	}
	if os.Getenv(shardTestExitEnv) == t.Name() {
		os.Exit(3)
	}
	if os.Getenv(shardTestFailEnv) == t.Name() {
		t.Fatal("synthetic test failure")
	}
}

func TestShardChildA(t *testing.T) { recordShardRun(t) }
func TestShardChildB(t *testing.T) { recordShardRun(t) }
func TestShardChildC(t *testing.T) { recordShardRun(t) }

// BenchmarkShardChildA matches the tests' -test.run pattern, which runs
// tests but never benchmarks.
func BenchmarkShardChildA(b *testing.B) {
	for b.Loop() {
	}
}

// TestShardChildSplit's nested names check that each element of a split
// subtest matches exactly: group/inner must not also run group/outer_inner.
func TestShardChildSplit(t *testing.T) {
	for _, name := range []string{"one", "two", "group/inner", "group/outer_inner"} {
		t.Run(name, recordShardRun)
	}
}

// meetShardTests answers the first two connections together, once both
// have arrived.
func meetShardTests(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		var connections []net.Conn
		for len(connections) < 2 {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections = append(connections, connection)
		}
		for _, connection := range connections {
			_, _ = connection.Write([]byte{1})
			_ = connection.Close()
		}
	}()
	return listener.Addr().String()
}

func TestRunShards(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("the child rendezvous requires two shard slots")
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		fail     string
		exit     string
		output   string
		children int
		logged   int
		// parallel is the run's -test.parallel value, 2 when unset.
		parallel int
	}{
		{name: "success", children: 2, logged: 7},
		// The run starts as many children as the machine's CPUs allow, up to
		// one for each test, so their number varies.
		{name: "divided parallel", parallel: 10, logged: 7},
		{name: "failure", fail: "TestShardChildB", output: "--- FAIL: TestShardChildB", children: 2, logged: 7},
		// The split subtest runs first, so the child that exits during it
		// later runs TestShardChildA or TestShardChildB in a new child. The
		// exited child's run leaves nothing in the test log.
		{name: "exit", exit: "TestShardChildSplit/one",
			output: "test shard exited while running ^TestShardChildSplit$/^one$", children: 3, logged: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parallel := cmp.Or(tc.parallel, 2)
			records := t.TempDir()
			testLog := filepath.Join(t.TempDir(), "testlog")
			command := exec.CommandContext(t.Context(), executable,
				"-test.run=^(?:Test|Benchmark)ShardChild(?:[ABC]|Split)$", "-test.parallel="+strconv.Itoa(parallel), "-test.v=true", "-test.testlogfile="+testLog)
			command.Env = append(os.Environ(), shardTestParentEnv+"=1", shardTestRecordEnv+"="+records,
				shardTestFailEnv+"="+tc.fail, shardTestExitEnv+"="+tc.exit, shardTestMeetEnv+"="+meetShardTests(t))
			output, err := command.CombinedOutput()
			if tc.output != "" {
				exitErr, ok := errors.AsType[*exec.ExitError](err)
				require.True(t, ok, "%v\n%s", err, output)
				require.Equal(t, 1, exitErr.ExitCode())
				require.Contains(t, string(output), tc.output)
			} else {
				require.NoError(t, err, string(output))
			}
			require.NotContains(t, string(output), "no tests to run")

			// Every selected test and split subtest ran once, outside the
			// parent, and a failure did not stop the others.
			entries, err := os.ReadDir(records)
			require.NoError(t, err)
			runs := map[string]string{}
			childParallel := map[string]int{}
			for _, entry := range entries {
				name, pid, found := strings.Cut(entry.Name(), "-")
				require.True(t, found, entry.Name())
				require.NotContains(t, runs, name, "test ran twice")
				runs[name] = pid
				value, err := os.ReadFile(filepath.Join(records, entry.Name()))
				require.NoError(t, err)
				childParallel[pid], err = strconv.Atoi(string(value))
				require.NoError(t, err)
			}
			require.ElementsMatch(t, []string{"TestShardChildA", "TestShardChildB", "TestShardChildC",
				"TestShardChildSplit.one", "TestShardChildSplit.two", "TestShardChildSplit.group.inner",
				"TestShardChildSplit.group.outer_inner"}, slices.Collect(maps.Keys(runs)))
			require.NotEqual(t, runs["TestShardChildA"], runs["TestShardChildB"])
			pids := slices.Compact(slices.Sorted(maps.Values(runs)))
			if tc.children != 0 {
				require.Len(t, pids, tc.children)
			}
			// Together the children run no more tests at once than -parallel
			// allows one process. TestShardChildA and TestShardChildB need two
			// children at once, so each may run at most half.
			for _, value := range childParallel {
				require.Positive(t, value)
				require.LessOrEqual(t, value, parallel/2, childParallel)
			}
			require.NotContains(t, pids, strconv.Itoa(command.Process.Pid))

			// go test keys its cache on one log: a single header, then each
			// test run's entries from the directory the children started in,
			// including runs after a child's first.
			data, err := os.ReadFile(testLog)
			require.NoError(t, err)
			require.True(t, bytes.HasPrefix(data, []byte("# test log\n")), string(data))
			require.Equal(t, 1, bytes.Count(data, []byte("# test log")), string(data))
			require.Equal(t, 7, bytes.Count(data, []byte("chdir "+wd+"\n")), string(data))
			require.Equal(t, tc.logged, bytes.Count(data, []byte("getenv "+shardTestRecordEnv+"\n")), string(data))
			require.NotContains(t, string(data), shardLogFlushEnv)
			require.True(t, bytes.HasSuffix(data, []byte("\n")), string(data))
		})
	}
}
