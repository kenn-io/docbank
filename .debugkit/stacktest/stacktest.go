// Package stacktest records goroutine stacks in test logs.
package stacktest

import (
	"bytes"
	"runtime/pprof"
	"testing"
)

// Dump synchronously logs every goroutine's stack to tb. Call it in a test's
// timeout branch before failing the test or canceling its blocked work.
//
// Illustrative example:
//
//	select {
//	case result := <-results:
//		// Check result.
//	case <-time.After(30 * time.Second):
//		stacktest.Dump(t)
//		require.FailNow(t, "operation timed out")
//	}
//
// Fail the test after Dump so the test runner shows the log.
// Call Dump while the test is still running; logging after completion panics.
// The log can be large when many goroutines are running.
func Dump(tb testing.TB) {
	tb.Helper()
	var stacks bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&stacks, 2) // bytes.Buffer writes cannot fail.
	tb.Logf("stacktest: goroutine stacks\n%s", stacks.String())
}
