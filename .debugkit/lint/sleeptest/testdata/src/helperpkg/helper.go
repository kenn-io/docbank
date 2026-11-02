// Package helperpkgtest is a test helper package: its name ends in "test",
// so non-test files are checked too.
package helperpkgtest

import (
	"testing"
	"testing/synctest"
	"time"
)

func WaitABit() {
	time.Sleep(time.Millisecond) // want "time.Sleep in a test outside a synctest bubble"
}

func RunInBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Second)
	})
}
