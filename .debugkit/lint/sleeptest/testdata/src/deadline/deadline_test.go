package deadline

import (
	"context"
	"deadline/helper"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const shortWait = 50 * time.Millisecond

func ready() bool { return true }

func TestShortBudgetsOutsideBubble(t *testing.T) {
	ctx := context.Background()
	_, cancel := context.WithTimeout(ctx, 5*time.Millisecond) // want "context.WithTimeout with budget 5ms in a test outside a synctest bubble races the wall clock"
	defer cancel()
	_, cancel = context.WithTimeoutCause(ctx, 10*time.Millisecond, errors.New("slow")) // want "context.WithTimeoutCause with budget 10ms"
	defer cancel()
	_, cancel = context.WithDeadline(ctx, time.Now().Add(50*time.Millisecond)) // want "context.WithDeadline with budget 50ms"
	defer cancel()
	_, cancel = context.WithDeadlineCause(ctx, time.Now().Add(shortWait), errors.New("slow")) // want "context.WithDeadlineCause with budget 50ms"
	defer cancel()
	<-time.After(75 * time.Millisecond)           // want "time.After with budget 75ms"
	timer := time.NewTimer(20 * time.Millisecond) // want "time.NewTimer with budget 20ms"
	defer timer.Stop()
	time.AfterFunc(shortWait, func() {}).Stop()                          // want "time.AfterFunc with budget 50ms"
	require.Eventually(t, ready, 300*time.Millisecond, time.Millisecond) // want "require.Eventually with budget 300ms"
	req := require.New(t)
	req.Eventually(ready, 100*time.Millisecond, time.Millisecond)                   // want "require.Eventually with budget 100ms"
	(*require.Assertions).Eventually(req, ready, 300*time.Millisecond, time.Second) // want "require.Eventually with budget 300ms"
	(require.Eventually)(t, ready, 200*time.Millisecond, time.Second)               // want "require.Eventually with budget 200ms"
	_, cancel = context.WithDeadline(ctx, (time.Now()).Add(60*time.Millisecond))    // want "context.WithDeadline with budget 60ms"
	defer cancel()
	_, cancel = context.WithDeadline(ctx, (time.Now().Add(70 * time.Millisecond))) // want "context.WithDeadline with budget 70ms"
	defer cancel()
	_, cancel = context.WithDeadline(ctx, (time.Now().Add)(90*time.Millisecond)) // want "context.WithDeadline with budget 90ms"
	defer cancel()
}

func TestZeroAndNegativePollingBudgets(t *testing.T) {
	require.Eventually(t, ready, 0, time.Millisecond)           // want "require.Eventually with budget 0s"
	assert.Never(t, ready, -time.Millisecond, time.Millisecond) // want "assert.Never with budget -1ms"
	require.Neverf(t, ready, 0, time.Millisecond, "idle")       // want "require.Neverf with budget 0s"
}

func TestShortBudgetsInsideBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		_, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
		defer cancel()
		<-time.After(75 * time.Millisecond)
		time.NewTimer(20 * time.Millisecond).Stop()
		require.Eventually(t, ready, 300*time.Millisecond, time.Millisecond)
		_ = []*config{{Timeout: shortWait}}
		c := config{}
		c.Timeout = shortWait
		helper.Wait(shortWait)
		_ = time.Now().Add(shortWait)
	})
}

func TestBudgetsThatPass(t *testing.T) {
	ctx := context.Background()
	wait := 50 * time.Millisecond
	_, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, cancel = context.WithTimeout(ctx, 0)
	defer cancel()
	_, cancel = context.WithTimeout(ctx, -time.Millisecond)
	defer cancel()
	_, cancel = context.WithTimeout(ctx, wait)
	defer cancel()
	_, cancel = context.WithDeadline(ctx, time.Now())
	defer cancel()
	assert.Never(t, ready, shortWait, time.Millisecond)
	require.Neverf(t, ready, 20*time.Millisecond, time.Millisecond, "idle")
}
