package telemetry

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdditionsHaveNoDailySuppression(t *testing.T) {
	enableTelemetryEnv(t)
	var mu sync.Mutex
	var events []map[string]any
	collector := screenCollector(t, &mu, &events)
	defer collector.Close()
	dir := t.TempDir()
	reporter := New(Options{Dir: dir, endpoint: collector.URL})
	defer func() { require.NoError(t, reporter.Close()) }()
	h := CaptureHandler(reporter, dir)
	for _, surface := range []string{"web", "tui", "cli"} {
		for range 2 {
			require.Equal(t, 202, postEvent(t, h, fmt.Sprintf(`{"event":"document_added","properties":{"surface":%q}}`, surface)).Code)
		}
	}
	require.NoError(t, reporter.Close())
	mu.Lock()
	require.Len(t, events, 6) // additions have no daily suppression
	mu.Unlock()
}
