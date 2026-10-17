package telemetry

import (
	_ "embed"
	"encoding/json/v2"
)

//go:embed search_types.json
var searchTypesJSON []byte

var searchTypes map[string]string

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

// DocumentAdded reports an addition the daemon has committed.
func (c *Capture) DocumentAdded(surface string) {
	if c == nil {
		return
	}
	_ = c.Capture(EventDocumentAdded, map[string]any{"surface": surface})
}

// TrackImport remembers the interface that started import id while this
// daemon runs. A later call for the same id keeps the first surface, and a
// restart forgets every import, so resumed work reports nothing.
func (c *Capture) TrackImport(id, surface string) {
	if c == nil || id == "" || surface == "" {
		return
	}
	c.importsMu.Lock()
	defer c.importsMu.Unlock()
	if c.imports == nil {
		c.imports = make(map[string]string)
	}
	if _, ok := c.imports[id]; !ok {
		c.imports[id] = surface
	}
}

// ImportAdded reports the first new document of a tracked import once, even
// when its drawer has closed.
func (c *Capture) ImportAdded(id string) {
	if c == nil {
		return
	}
	c.importsMu.Lock()
	surface := c.imports[id]
	if surface != "" {
		c.imports[id] = ""
	}
	c.importsMu.Unlock()
	if surface != "" {
		c.DocumentAdded(surface)
	}
}

// ForgetImport releases an import that reached a terminal state.
func (c *Capture) ForgetImport(id string) {
	if c == nil {
		return
	}
	c.importsMu.Lock()
	defer c.importsMu.Unlock()
	delete(c.imports, id)
}
