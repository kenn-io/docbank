package posthog

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSendGroupWaitAfterReuse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var group SendGroup
		group.Go(func() {})
		synctest.Wait()
		group.Go(func() {
			time.Sleep(5 * time.Millisecond)
		})

		start := time.Now()
		group.Wait(50 * time.Millisecond)

		assert.Equal(t, 5*time.Millisecond, time.Since(start))
	})
}

func TestSendGroupWaitSharesLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var group SendGroup
		release := make(chan struct{})
		delivered := make(chan struct{}, 2)
		for range 2 {
			group.Go(func() {
				<-release
				delivered <- struct{}{}
			})
		}

		start := time.Now()
		group.Wait(50 * time.Millisecond)

		assert.Equal(t, 50*time.Millisecond, time.Since(start))
		assert.Empty(t, delivered)
		close(release)
		group.Wait(time.Second)
		assert.Len(t, delivered, 2, "sends continue after the wait limit")
	})
}

func TestSendGroupWaitEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var group SendGroup

		start := time.Now()
		group.Wait(50 * time.Millisecond)

		assert.Zero(t, time.Since(start))
	})
}
