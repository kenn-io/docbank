package processing

import (
	"bytes"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// fencedRevocation is one consent revocation expected to wait for an open
// provider-egress fence. The store holds that fence in a sync.RWMutex, which
// does not durably block a synctest bubble, so the test reads the waiting
// goroutine's stack instead of waiting on a clock.
type fencedRevocation struct {
	header []byte
	done   chan error
}

func startFencedRevocation(revoke func() error) *fencedRevocation {
	started := make(chan []byte, 1)
	revocation := &fencedRevocation{done: make(chan error, 1)}
	go func() {
		buf := make([]byte, 64)
		buf = buf[:runtime.Stack(buf, false)]
		// "goroutine N [running]:" names this goroutine in a full dump.
		started <- buf[:bytes.IndexByte(buf, '[')]
		revocation.done <- revoke()
	}()
	revocation.header = <-started
	return revocation
}

// revokeConsentParkedOnFence matches RevokeConsent blocked acquiring the
// write side of the store's provider-egress fence.
var revokeConsentParkedOnFence = regexp.MustCompile(
	`\Async\.RWMutex\.Lock[^\n]*\n(?:[^\n]*\n)*?sync\.\(\*RWMutex\)\.Lock\([^\n]*\n\t[^\n]*\n` +
		`go\.kenn\.io/docbank/internal/store\.\(\*Store\)\.RevokeConsent\(`)

// requireWaiting returns once the revocation is parked on the fence. It fails
// as soon as the revocation returns instead, because then it crossed the fence.
func (revocation *fencedRevocation) requireWaiting(t *testing.T, crossed string) {
	t.Helper()
	buf := make([]byte, 1<<16)
	for {
		select {
		case err := <-revocation.done:
			require.FailNow(t, crossed, "revocation returned: %v", err)
		default:
		}
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		for goroutine := range bytes.SplitSeq(buf[:n], []byte("\n\n")) {
			state, ok := bytes.CutPrefix(goroutine, revocation.header)
			if ok && state[0] == '[' && revokeConsentParkedOnFence.Match(state[1:]) {
				return
			}
		}
		runtime.Gosched()
	}
}
