package packagetest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// shardChildEnv marks a process RunShards started to run part of the tests.
const shardChildEnv = "DOCBANK_TEST_SHARD_CHILD"

// shardMarkerEnv carries the text a child writes after each test it runs.
const shardMarkerEnv = "DOCBANK_TEST_SHARD_MARKER"

// shardDeadlineEnv carries the package's -test.timeout deadline to children.
const shardDeadlineEnv = "DOCBANK_TEST_SHARD_DEADLINE"

// slotPollInterval is how often a worker retries its slot while it waits;
// Lock cannot be canceled when the package deadline passes.
const slotPollInterval = 10 * time.Millisecond

// testLogHeader starts every test log; cmd/go reads it to key the test cache.
const testLogHeader = "# test log\n"

// RunShards runs the package's tests in several child processes of the test
// binary at once, at most -test.parallel of them, and returns the exit code.
// A child runs one test at a time, so it gets one CPU. When -parallel is set,
// the children divide it, so parallel subtests in all of them together run no
// more tests at once than one process would.
//
// Use it for packages whose tests run serially, for example because they
// change environment variables, the working directory, command-line globals or
// another process-wide resource. Each child runs a subset of the
// top-level tests serially, exactly as a single process would; every selected
// test still runs once. A child starts only when a slot is free and then runs
// one test after another, so its process-wide setup serves several tests.
// Each test's output is relayed whole after it finishes, so go test -json and
// gotestsum see its events together.
//
// Each split names a top-level test whose listed subtests run one at a time,
// each as its own run of the top-level test, so the children share them like
// separate tests. go test reports the top-level test once for each of its
// runs; each subtest still runs once.
// Use it for a test whose subtests dominate the package, and list every
// subtest that it starts.
//
// -test.timeout bounds the whole run, as in one process. A test still running
// at the deadline panics in its child, which names it, before go test kills
// the package.
//
// It runs the tests in this process instead when sharding cannot preserve the
// run's meaning: listing, benchmarks, fuzzing, profiles, coverage, a -run
// pattern that selects subtests, or fewer than two selected tests.
func RunShards(m *testing.M, splits ...Split) int {
	if !flag.Parsed() {
		flag.Parse()
	}
	if os.Getenv(shardChildEnv) == "1" {
		if marker := os.Getenv(shardMarkerEnv); marker != "" {
			return serveShards(m, marker)
		}
		return m.Run()
	}
	// The parent never calls m.Run, so no -test.timeout timer runs here, and
	// each m.Run in a child times only its own call. Without one deadline for
	// the package, go test kills the parent a minute after -test.timeout and
	// the hung test's name and output are lost.
	timeout, err := time.ParseDuration(flagValue("test.timeout"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading -test.timeout: %v\n", err)
		return 2
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	// TestMain has no caller context; the deadline bounds the children.
	ctx := context.Background()
	names, ok := shardableTests(ctx)
	if !ok {
		return m.Run()
	}
	return runShards(ctx, names, splits, deadline)
}

// Split names a top-level test and subtests it starts. A subtest may name a
// nested subtest with slashes, as go test -run does.
type Split struct {
	Test     string
	Subtests []string
}

func shardableTests(ctx context.Context) ([]string, bool) {
	for _, name := range []string{"test.list", "test.bench", "test.fuzz", "test.coverprofile", "test.cpuprofile",
		"test.memprofile", "test.blockprofile", "test.mutexprofile", "test.trace", "test.gocoverdir"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			return nil, false
		}
	}
	if testing.CoverMode() != "" {
		return nil, false
	}
	pattern := flagValue("test.run")
	if strings.Contains(pattern, "/") {
		return nil, false
	}
	if pattern == "" {
		pattern = "."
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, false
	}
	command := exec.CommandContext(ctx, executable, "-test.list="+pattern) //nolint:gosec // executable is the running test binary.
	command.Env = append(os.Environ(), shardChildEnv+"=1")
	out, err := command.Output()
	if err != nil {
		return nil, false
	}
	// -test.run never selects benchmarks, which go test names Benchmark*.
	names := slices.DeleteFunc(strings.Fields(string(out)), func(name string) bool {
		return strings.HasPrefix(name, "Benchmark")
	})
	if len(names) < 2 {
		return nil, false
	}
	return names, true
}

func flagValue(name string) string {
	if f := flag.Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}

func runShards(ctx context.Context, names []string, splits []Split, deadline time.Time) int {
	subtests := map[string][]string{}
	for _, split := range splits {
		subtests[split.Test] = split.Subtests
	}
	// Split tests start first: they are the package's largest. Each pattern
	// selects one test, so idle children take the next one as they finish.
	var patterns, plain []string
	for _, name := range names {
		if len(subtests[name]) == 0 {
			plain = append(plain, "^"+regexp.QuoteMeta(name)+"$")
			continue
		}
		for _, subtest := range subtests[name] {
			// -test.run matches each slash-separated element of a name
			// separately, so each element is anchored on its own.
			var pattern strings.Builder
			pattern.WriteString("^" + regexp.QuoteMeta(name) + "$")
			for element := range strings.SplitSeq(subtest, "/") {
				pattern.WriteString("/^" + regexp.QuoteMeta(element) + "$")
			}
			patterns = append(patterns, pattern.String())
		}
	}
	patterns = append(patterns, plain...)
	workers := len(patterns)
	parallel, err := strconv.Atoi(flagValue("test.parallel"))
	if err == nil && parallel > 0 {
		workers = min(workers, parallel)
	}
	// go test runs many packages at once, and each sharded package would
	// otherwise assume it has the whole machine. A child runs a test only
	// while it holds a slot, and children across all packages share one slot
	// for each CPU.
	slots := runtime.NumCPU()
	workers = min(workers, max(2, slots))

	testLog := flagValue("test.testlogfile")
	var baseArgs []string
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "test.run", "test.testlogfile", "test.timeout":
		case "test.parallel":
			// -parallel bounds the tests that run at once in the package, as
			// in one process: tests that hold a server connection or another
			// shared resource rely on it. The children divide it.
			baseArgs = append(baseArgs, "-test.parallel="+strconv.Itoa(max(1, parallel/workers)))
		default:
			baseArgs = append(baseArgs, "-"+f.Name+"="+f.Value.String())
		}
	})
	logDir := ""
	if testLog != "" {
		dir, err := os.MkdirTemp("", "docbank-test-shard-logs-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "creating shard test logs: %v\n", err)
			return 1
		}
		logDir = dir
		defer func() { _ = os.RemoveAll(logDir) }()
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "finding test binary: %v\n", err)
		return 1
	}
	// The directory admits only its creator, so each user needs their own
	// when several share a temporary directory such as /tmp. Windows reports
	// -1 here, and its temporary directory is already per user.
	slotDir := filepath.Join(os.TempDir(), "docbank-test-shard-slots-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(slotDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "creating shard slots: %v\n", err)
		return 1
	}
	marker := rand.Text()
	childEnv := append(os.Environ(), shardChildEnv+"=1", shardMarkerEnv+"="+marker)
	if !deadline.IsZero() {
		childEnv = append(childEnv, shardDeadlineEnv+"="+deadline.Format(time.RFC3339Nano))
	}
	if _, set := os.LookupEnv("GOMAXPROCS"); !set {
		childEnv = append(childEnv, "GOMAXPROCS=1")
	}
	pool := &shardPool{executable: executable, baseArgs: baseArgs, env: childEnv, marker: marker, logDir: logDir,
		deadline: deadline, failFast: flagValue("test.failfast") == "true"}
	queue := make(chan string, len(patterns))
	for _, pattern := range patterns {
		queue <- pattern
	}
	close(queue)
	var wg sync.WaitGroup
	for w := range workers {
		// Each goroutine holds its own handle to every slot: a process's
		// handles to one file exclude each other, and a handle is not shared.
		handles := make([]*flock.Flock, slots)
		for i := range handles {
			handles[i] = flock.New(filepath.Join(slotDir, strconv.Itoa(i)))
		}
		wg.Go(func() { pool.work(ctx, handles, (os.Getpid()+w)%slots, queue) })
	}
	wg.Wait()

	if testLog != "" {
		if err := mergeTestLogs(testLog, logDir, pool.started); err != nil {
			fmt.Fprintf(os.Stderr, "writing test log: %v\n", err)
			return 1
		}
	}
	if pool.failed {
		return 1
	}
	return 0
}

// shardPool starts the children and relays their output.
type shardPool struct {
	executable string
	baseArgs   []string
	env        []string
	marker     string
	logDir     string
	deadline   time.Time
	failFast   bool

	mu      sync.Mutex
	failed  bool
	started int
}

// work runs tests from queue, each while it holds one of the shared slots.
// It starts a child only when it first gets a test and a slot, so a package
// starts no more children than the slots it gets, and each child keeps its
// process-wide setup, such as compiled code, for the tests it runs later.
func (p *shardPool) work(ctx context.Context, slots []*flock.Flock, own int, queue <-chan string) {
	var child *shardChild
	defer func() {
		if child != nil {
			p.finish(child.stop())
		}
	}()
	for pattern := range queue {
		if p.stopping() {
			return
		}
		slot, err := acquireSlot(ctx, slots, own, p.deadline)
		if err != nil {
			p.finish(nil, fmt.Errorf("locking shard slot: %w", err))
			return
		}
		if child == nil {
			if child, err = p.start(ctx); err != nil {
				_ = slot.Unlock()
				p.finish(nil, fmt.Errorf("starting test shard: %w", err))
				return
			}
		}
		out, err := child.run(pattern)
		if unlockErr := slot.Unlock(); unlockErr != nil {
			p.finish(nil, fmt.Errorf("unlocking shard slot: %w", unlockErr))
		}
		if child.done {
			// The child exited before finishing the test; a later test gets
			// a new child.
			rest, waitErr := child.stop()
			out = append(out, rest...)
			err = errors.Join(err, waitErr)
			child = nil
		}
		p.finish(out, err)
	}
}

// acquireSlot locks the first free slot, starting at own, or waits for own
// when every slot is busy, until deadline when it is set.
func acquireSlot(ctx context.Context, slots []*flock.Flock, own int, deadline time.Time) (*flock.Flock, error) {
	for i := range slots {
		slot := slots[(own+i)%len(slots)]
		locked, err := slot.TryLock()
		if err != nil {
			return nil, fmt.Errorf("trying shard slot %s: %w", slot.Path(), err)
		}
		if locked {
			return slot, nil
		}
	}
	slot := slots[own]
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	if _, err := slot.TryLockContext(ctx, slotPollInterval); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("waiting for shard slot %s: the package's -test.timeout passed", slot.Path())
		}
		return nil, fmt.Errorf("waiting for shard slot %s: %w", slot.Path(), err)
	}
	return slot, nil
}

func (p *shardPool) stopping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed && p.failFast
}

func (p *shardPool) start(ctx context.Context) (*shardChild, error) {
	p.mu.Lock()
	index := p.started
	p.started++
	p.mu.Unlock()
	args := p.baseArgs
	if p.logDir != "" {
		args = append(append([]string(nil), args...), "-test.testlogfile="+filepath.Join(p.logDir, strconv.Itoa(index)))
	}
	command := exec.CommandContext(ctx, p.executable, args...) //nolint:gosec // executable is the running test binary.
	command.Env = p.env
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("connecting test shard input: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("connecting test shard output: %w", err)
	}
	// One pipe keeps stdout and stderr in the order the child wrote them.
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("starting test shard: %w", err)
	}
	return &shardChild{command: command, stdin: stdin, output: bufio.NewReader(stdout), marker: []byte(p.marker + " ")}, nil
}

// finish relays one test's output whole, so go test -json and gotestsum see
// its events together, and records a failure.
func (p *shardPool) finish(out []byte, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = os.Stdout.Write(out)
	if err != nil {
		p.failed = true
		if !errors.Is(err, errShardTestFailed) {
			_, _ = fmt.Fprintf(os.Stdout, "running test shard: %v\nFAIL\n", err)
		}
	}
}

// errShardTestFailed reports a test that failed; its output says why.
var errShardTestFailed = errors.New("test failed")

// shardChild is a test binary that runs one -test.run pattern for each line
// it reads; serveShards is its side.
type shardChild struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	output  *bufio.Reader
	marker  []byte
	done    bool
}

// run sends pattern to the child and returns the output of that run, or
// errShardTestFailed when the test failed.
func (c *shardChild) run(pattern string) ([]byte, error) {
	if _, err := io.WriteString(c.stdin, pattern+"\n"); err != nil {
		c.done = true
		return nil, fmt.Errorf("sending %s to test shard: %w", pattern, err)
	}
	var out []byte
	for {
		line, err := c.output.ReadBytes('\n')
		if before, after, found := bytes.Cut(line, c.marker); found {
			out = append(out, before...)
			code, convErr := strconv.Atoi(string(bytes.TrimSpace(after)))
			if convErr != nil {
				return out, fmt.Errorf("reading test shard result for %s: %w", pattern, convErr)
			}
			if code != 0 {
				return out, errShardTestFailed
			}
			return out, nil
		}
		out = append(out, line...)
		if err != nil {
			c.done = true
			return out, fmt.Errorf("test shard exited while running %s", pattern)
		}
	}
}

// stop ends the child after its last test and returns what it wrote since.
func (c *shardChild) stop() ([]byte, error) {
	closeErr := c.stdin.Close()
	rest, readErr := io.ReadAll(c.output)
	return rest, errors.Join(readErr, c.command.Wait(), closeErr)
}

// serveShards runs each -test.run pattern read from standard input with
// m.Run, which go test documents may run more than once, then writes the
// parent's marker and the run's exit code on a line of their own.
func serveShards(m *testing.M, marker string) int {
	// Tests that start this binary as a helper must not serve tests too.
	if err := os.Unsetenv(shardMarkerEnv); err != nil {
		fmt.Fprintf(os.Stderr, "clearing %s: %v\n", shardMarkerEnv, err)
		return 2
	}
	var deadline time.Time
	if value := os.Getenv(shardDeadlineEnv); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading %s: %v\n", shardDeadlineEnv, err)
			return 2
		}
		deadline = parsed
	}
	testLog := flagValue("test.testlogfile")
	lines := bufio.NewScanner(os.Stdin)
	for run := 0; lines.Scan(); run++ {
		if err := flag.Set("test.run", lines.Text()); err != nil {
			fmt.Fprintf(os.Stderr, "selecting tests: %v\n", err)
			return 2
		}
		if testLog != "" && run > 0 {
			// Later calls append to an existing log, so start each one.
			path := testLog + "." + strconv.Itoa(run)
			if err := errors.Join(os.WriteFile(path, nil, 0o600), flag.Set("test.testlogfile", path)); err != nil {
				fmt.Fprintf(os.Stderr, "starting test log: %v\n", err)
				return 2
			}
		}
		if !deadline.IsZero() {
			// m.Run times each call from its start, so it gets only what is
			// left of the package's time. A zero timeout would mean none.
			if err := flag.Set("test.timeout", max(time.Until(deadline), time.Nanosecond).String()); err != nil {
				fmt.Fprintf(os.Stderr, "setting test timeout: %v\n", err)
				return 2
			}
		}
		code := m.Run()
		if testLog != "" {
			flushTestLog()
		}
		fmt.Printf("%s %d\n", marker, code)
	}
	if err := lines.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "reading tests to run: %v\n", err)
		return 1
	}
	return 0
}

// shardLogFlushEnv is an unset variable whose lookups flush the test log.
const shardLogFlushEnv = "DOCBANK_TEST_SHARD_LOG_FLUSH"

// flushTestLog writes out the test log entries of the m.Run call that just
// returned. testing writes the log only after the first call; later calls
// keep their entries in a buffer that is written only when it fills. Looking
// up a variable adds an entry, and enough of them fill any buffer up to 64
// KiB. mergeTestLogs removes them, with the incomplete one that stays in the
// buffer.
func flushTestLog() {
	entry := len("getenv " + shardLogFlushEnv + "\n")
	for range (64<<10)/entry + 1 {
		_ = os.Getenv(shardLogFlushEnv)
	}
}

// mergeTestLogs combines the children's test logs into the one go test reads.
// Child i writes its first run's log to dir/i and each later run's log to
// dir/i.N. cmd/go resolves relative paths against the latest chdir, so each
// log's entries start from the directory every child started in.
func mergeTestLogs(path, dir string, children int) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	flush := []byte("getenv " + shardLogFlushEnv)
	merged := []byte(testLogHeader)
	for child := range children {
		for run := 0; ; run++ {
			name := strconv.Itoa(child)
			if run > 0 {
				name += "." + strconv.Itoa(run)
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			merged = fmt.Appendf(merged, "chdir %s\n", wd)
			data = bytes.TrimPrefix(data, []byte(testLogHeader))
			// A log ends in a flush entry cut short in the buffer, or in the
			// newline of the last entry.
			data = data[:bytes.LastIndexByte(data, '\n')+1]
			for line := range bytes.Lines(data) {
				if !bytes.Equal(bytes.TrimSuffix(line, []byte("\n")), flush) {
					merged = append(merged, line...)
				}
			}
		}
	}
	return os.WriteFile(path, merged, 0o600)
}
