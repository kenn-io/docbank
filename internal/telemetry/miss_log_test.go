package telemetry

import (
	"reflect"
	"testing"
)

// Search history remains private even when a caller sends receipt fields with
// the ordinary anonymous search_empty action.
func FuzzSearchEmptyExcludesMissLogReceipt(f *testing.F) {
	f.Setenv(EnabledEnv, "0")
	reporter := New(Options{Dir: f.TempDir(), Logger: discardLogger()})
	f.Cleanup(func() { _ = reporter.Close() })
	want, err := reporter.SanitizeProperties(EventSearchEmpty, map[string]any{"surface": "cli", "search_type": "text"})
	if err != nil {
		f.Fatal(err)
	}
	f.Add("synthetic private question", "/synthetic/document.txt", "synthetic correction")
	f.Add("quote,\"newline\nquestion", "", "")
	f.Fuzz(func(t *testing.T, query, path, note string) {
		properties, err := reporter.SanitizeProperties(EventSearchEmpty, map[string]any{
			"surface": "cli", "search_type": "text", "query": query,
			"path": path, "note": note, "miss_log_id": "synthetic-record",
			"receipt": map[string]any{"query": query, "path": path, "note": note},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(properties, want) {
			t.Fatal("search_empty forwarded fields outside its anonymous allowlist")
		}
	})
}
