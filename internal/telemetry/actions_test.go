package telemetry

import (
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionEventsEndpoint(t *testing.T) {
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
		for _, event := range []string{"document_added", "document_opened", "search_run"} {
			values := []string{""}
			if event == "search_run" {
				values = []string{"text", "semantic", "hybrid"}
			}
			for _, value := range values {
				body := fmt.Sprintf(`{"event":%q,"properties":{"surface":%q,"search_type":%q}}`, event, surface, value)
				for range 2 {
					require.Equal(t, 202, postEvent(t, h, body).Code)
				}
			}
			for _, props := range []string{`{}`, `{"surface":"invalid"}`, `{"surface":42}`} {
				require.Equal(t, 400, postEvent(t, h, fmt.Sprintf(`{"event":%q,"properties":%s}`, event, props)).Code)
			}
		}
	}
	for _, props := range []string{`{"surface":"web"}`, `{"surface":"web","search_type":"auto"}`, `{"surface":"web","search_type":42}`} {
		require.Equal(t, 400, postEvent(t, h, `{"event":"search_run","properties":`+props+`}`).Code)
	}
	imports := NewImports(reporter)
	admission := func() (string, bool, error) { return "mailbox:synthetic", true, nil }
	require.NoError(t, imports.Admit("web", admission))
	imports.Result("mailbox:synthetic", false, false)
	require.NoError(t, imports.Admit("cli", admission))
	imports.Result("mailbox:synthetic", true, false)
	imports.Result("mailbox:synthetic", true, true)
	require.NoError(t, imports.Admit("web", func() (string, bool, error) { return "photos:lost", true, nil }))
	NewImports(reporter).Result("photos:lost", true, true)
	t.Setenv(EnabledEnv, "0")
	imports = NewImports(New(Options{Dir: t.TempDir()}))
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
	require.Len(t, events, 19) // six additions, twelve daily claims, one attributed import
	assert.Equal(t, "web", events[len(events)-1]["surface"])
}
