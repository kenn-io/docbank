package telemetry

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportAttributionObservationAndCapacity(t *testing.T) {
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
	events = nil
	mu.Unlock()
	reporter = New(Options{Dir: dir, endpoint: collector.URL})
	imports := NewImports(CaptureHandler(reporter, dir))
	admission := func() (string, bool, error) { return "mailbox:synthetic", true, nil }
	require.NoError(t, imports.Admit("web", admission))
	imports.Result("mailbox:synthetic", false, false)
	require.NoError(t, imports.Admit("cli", admission))
	imports.Result("mailbox:synthetic", true, false)
	imports.Result("mailbox:synthetic", true, true)
	require.NoError(t, imports.Admit("web", func() (string, bool, error) { return "mailbox:second", true, nil }))
	imports.Result("mailbox:second", true, true)
	require.NoError(t, imports.Admit("web", func() (string, bool, error) { return "photos:lost", true, nil }))
	NewImports(CaptureHandler(reporter, dir)).Result("photos:lost", true, true)
	gateCalls, reads := 0, 0
	gate := func(_ context.Context, fn func() error) error { gateCalls++; return fn() }
	read := func() (int64, bool, error) { reads++; return 1, false, nil }
	require.NoError(t, imports.Observe(t.Context(), "untracked", gate, read))
	assert.Equal(t, 0, gateCalls, "untracked progress must skip the mutation gate")
	assert.Equal(t, 0, reads, "untracked progress must skip storage")
	require.NoError(t, imports.Admit("web", func() (string, bool, error) { return "tracked", true, nil }))
	gateCalls, reads = 0, 0
	require.NoError(t, imports.Observe(t.Context(), "tracked", gate, read))
	require.Equal(t, 1, gateCalls)
	require.Equal(t, 1, reads)
	require.NoError(t, imports.Observe(t.Context(), "tracked", gate, read))
	assert.Equal(t, 1, gateCalls, "reported progress must skip the mutation gate")
	assert.Equal(t, 1, reads, "reported progress must skip storage")
	t.Setenv(EnabledEnv, "0")
	imports = NewImports(CaptureHandler(New(Options{Dir: t.TempDir()}), t.TempDir()))
	admitted := 0
	for i := range 257 {
		require.NoError(t, imports.Admit("cli", func() (string, bool, error) {
			admitted++
			return strconv.Itoa(i), true, nil
		}))
	}
	assert.Equal(t, 257, admitted)
	assert.Len(t, imports.entries, 256)
	imports.Result("0", false, true)
	require.NoError(t, imports.Admit("cli", func() (string, bool, error) { return "next", true, nil }))
	assert.Len(t, imports.entries, 256)
	assert.Contains(t, imports.entries, "next")
	require.NoError(t, reporter.Close())
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 3) // each import reports its first attributed addition
	assert.Equal(t, "web", events[len(events)-1]["surface"])
}
