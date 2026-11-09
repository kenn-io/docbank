//go:build linux || darwin

package conversionworker

import (
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkerReportsRetainedSourceStorageFailureAsUnavailable(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)

	var previous syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous))
	restricted := previous
	if restricted.Cur > 1 {
		restricted.Cur = 1
	}
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() {
		restoreErr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous)
		signal.Reset(syscall.SIGXFSZ)
		require.NoError(t, restoreErr)
	})
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &restricted))

	response := submitRequest(t, w, strings.Repeat("a", 64), []byte("synthetic source"))
	require.Contains(t, []int{http.StatusServiceUnavailable, http.StatusInsufficientStorage}, response.Code, response.Body.String())
}
