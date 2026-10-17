package telemetry

import (
	_ "embed"
	"encoding/json/v2"
)

//go:embed search_types.json
var searchTypesJSON []byte

var searchTypes map[string]string

// Enabled reports whether this daemon can send usage events.
func (c *Capture) Enabled() bool {
	return c != nil && c.reporter.Enabled()
}

func init() {
	if err := json.Unmarshal(searchTypesJSON, &searchTypes); err != nil {
		panic(err)
	}
}

// SearchType groups the executed search path without sending query text.
func SearchType(mode string) string {
	if value, ok := searchTypes[mode]; ok {
		return value
	}
	return searchTypes["default"]
}

// DocumentAdded reports an addition the daemon has committed. An empty
// surface, from an import no interface started, reports nothing.
func (c *Capture) DocumentAdded(surface string) {
	if c == nil || surface == "" {
		return
	}
	_ = c.Capture(EventDocumentAdded, map[string]any{"surface": surface})
}
