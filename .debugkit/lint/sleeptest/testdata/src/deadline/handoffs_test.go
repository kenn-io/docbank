package deadline

import (
	"deadline/helper"
	"deadlineother"
	"net/http"
	"testing"
	"time"
)

type durationAlias = time.Duration
type config struct {
	Timeout   durationAlias
	RetryWait int
}

type client struct {
	HTTPTimeout time.Duration
	TicketTTL   time.Duration
	Awaited     time.Duration
}

type backOff struct {
	InitialInterval time.Duration
	MaxInterval     time.Duration
	RetryDelay      time.Duration
}

func (config) Wait(deadline time.Duration)                      {}
func probe(t *testing.T, tick time.Duration)                    {}
func watcher(n int, batchDelay time.Duration, backend any)      {}
func waits(minInterval time.Duration, budgets ...time.Duration) {}

type fakeClock struct{}

func (fakeClock) Advance(d time.Duration) {}

const watcherInterval = 100 * time.Millisecond

func TestHandoffs(t *testing.T) {
	_ = http.Client{Timeout: 20 * time.Millisecond}              // want "Timeout with budget 20ms"
	c := config{20 * time.Millisecond, 20}                       // want "Timeout with budget 20ms"
	_ = []*config{{Timeout: shortWait}}                          // want "Timeout with budget 50ms"
	c.Timeout = 30 * time.Millisecond                            // want "Timeout with budget 30ms"
	_ = client{HTTPTimeout: shortWait}                           // want "HTTPTimeout with budget 50ms"
	waits(time.Second, 10*time.Millisecond, 20*time.Millisecond) // want "waits with budget 10ms" "waits with budget 20ms"
	c.Wait(shortWait)                                            // want "Wait with budget 50ms"
	config.Wait(c, shortWait)                                    // want "Wait with budget 50ms"
	helper.Wait(shortWait)                                       // want "Wait with budget 50ms"
	_ = time.Now().Add(50 * time.Millisecond)                    // want "time.Now\\(\\).Add with budget 50ms"

	_ = config{RetryWait: 20}
	_ = client{TicketTTL: shortWait, Awaited: shortWait}
	_ = backOff{InitialInterval: time.Millisecond, MaxInterval: shortWait, RetryDelay: shortWait}
	probe(t, time.Millisecond)
	watcher(0, shortWait, nil)
	waits(watcherInterval)
	_ = struct{ Age time.Duration }{shortWait}
	fakeClock{}.Advance(shortWait)
	c.Timeout += shortWait
	c.RetryWait = 20
	deadlineother.Wait(shortWait)
}
