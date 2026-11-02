package stacktest_test

import (
	"os"
	"os/exec"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
)

func blockedWorker(release <-chan struct{}) { <-release }

type blockingTB struct {
	testing.TB
	logging chan<- struct{}
	release <-chan struct{}
}

func (tb *blockingTB) Logf(string, ...any) {
	close(tb.logging)
	<-tb.release
}

func TestDumpWaitsForLogging(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logging := make(chan struct{})
		release := make(chan struct{})
		tb := &blockingTB{TB: t, logging: logging, release: release}
		returned := make(chan struct{})
		go func() {
			stacktest.Dump(tb)
			close(returned)
		}()
		<-logging
		synctest.Wait()
		select {
		case <-returned:
			assert.Fail(t, "Dump returned while Logf was blocked")
		default:
		}
		close(release)
		<-returned
	})
}

func TestDumpFailureOutput(t *testing.T) {
	if os.Getenv("STACKTEST_FAILURE_HELPER") == "1" {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			go blockedWorker(release)
			synctest.Wait()
			stacktest.Dump(t)
			require.FailNow(t, "operation timed out")
		})
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestDumpFailureOutput$")
	cmd.Env = append(os.Environ(), "STACKTEST_FAILURE_HELPER=1")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 1, exit.ExitCode(), "%s", output)
	log := string(output)
	assert.Contains(t, log, "stacktest_test.blockedWorker")
	assert.Regexp(t, `stacktest_test\.go:\d+: stacktest: goroutine stacks`, log)
}
