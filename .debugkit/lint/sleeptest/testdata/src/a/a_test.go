package a

import (
	"testing"
	"testing/synctest"
	"time"

	clock "time"
)

func TestSleepsForReal(t *testing.T) {
	time.Sleep(10 * time.Millisecond) // want "time.Sleep in a test outside a synctest bubble"
}

func TestSleepsInsideParenthesizedBubble(t *testing.T) {
	synctest.Test(t, (func(t *testing.T) {
		time.Sleep(time.Second)
	}))
}

func TestSleepsInsideBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Second)
		func() {
			time.Sleep(time.Second)
		}()
	})
}

func TestSleepsInSubtestOutsideBubble(t *testing.T) {
	t.Run("sub", func(t *testing.T) {
		time.Sleep(time.Millisecond) // want "time.Sleep in a test outside a synctest bubble"
	})
	synctest.Test(t, func(t *testing.T) {
		t.Run("inner", func(t *testing.T) {
			time.Sleep(time.Millisecond)
		})
	})
}

func waitHelper(t *testing.T) {
	t.Helper()
	time.Sleep(time.Millisecond) // want "time.Sleep in a test outside a synctest bubble"
}

func TestOtherTimeCallsAreFine(t *testing.T) {
	_ = time.Now()
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	<-time.After(0)
}

func bubbleBody(t *testing.T) {
	time.Sleep(time.Second)
}

func mixedBubble(t *testing.T) {
	time.Sleep(time.Second) // want "time.Sleep in a test outside a synctest bubble"
}

var packageBubble = func(t *testing.T) {
	time.Sleep(time.Second)
}

func TestNamedCallbacksRunInsideBubble(t *testing.T) {
	synctest.Test(t, bubbleBody)
	synctest.Test(t, packageBubble)
	local := func(t *testing.T) {
		time.Sleep(time.Second)
	}
	synctest.Test(t, local)
	var paren = (func(t *testing.T) {
		time.Sleep(time.Second)
	})
	synctest.Test(t, paren)
	var declared func(*testing.T)
	declared = func(t *testing.T) {
		time.Sleep(time.Millisecond) // want "time.Sleep in a test outside a synctest bubble"
	}
	synctest.Test(t, declared)
	synctest.Test(t, mixedBubble)
	mixedBubble(t)
}

func TestSleepInGoroutineInsideBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			time.Sleep(time.Second)
			close(done)
		}()
		<-done
	})
}

func TestSleepInGoroutineOutsideBubble(t *testing.T) {
	done := make(chan struct{})
	go func() {
		time.Sleep(time.Millisecond) // want "time.Sleep in a test outside a synctest bubble"
		close(done)
	}()
	<-done
}

func TestSleepThroughAlias(t *testing.T) {
	clock.Sleep(time.Millisecond) // want "time.Sleep in a test outside a synctest bubble"
}

func TestHelperCalledFromBubbleIsStillReported(t *testing.T) {
	// waitHelper sleeps and is reported at its own body; calls from inside a
	// bubble cannot lift that (documented limitation).
	synctest.Test(t, func(t *testing.T) {
		waitHelper(t)
	})
}
