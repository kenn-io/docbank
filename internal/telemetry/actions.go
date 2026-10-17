package telemetry

import (
	_ "embed"
	"encoding/json/v2"
	"slices"
)

const maxTrackedImports = 256

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
// The registry keeps at most 256 entries. Under pressure it evicts the oldest
// completed claim, or drops new attribution while every entry is active.
// Push clients complete their claims only after every upload has finished.
func (c *Capture) TrackImport(id, surface string) {
	if c == nil || id == "" || surface == "" {
		return
	}
	c.importsMu.Lock()
	defer c.importsMu.Unlock()
	if c.imports == nil {
		c.imports = make(map[string]string)
	}
	if _, ok := c.imports[id]; ok {
		return
	}
	if len(c.imports) >= maxTrackedImports {
		if len(c.completedImports) == 0 {
			return
		}
		delete(c.imports, c.completedImports[0])
		c.completedImports = c.completedImports[1:]
	}
	c.imports[id] = surface
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

// CompleteImport makes a finished push claim eligible for oldest-first eviction.
// Repeated and unknown completions leave the registry unchanged.
func (c *Capture) CompleteImport(id string) {
	if c == nil {
		return
	}
	c.importsMu.Lock()
	defer c.importsMu.Unlock()
	if _, ok := c.imports[id]; ok && !slices.Contains(c.completedImports, id) {
		c.completedImports = append(c.completedImports, id)
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
	if index := slices.Index(c.completedImports, id); index >= 0 {
		c.completedImports = slices.Delete(c.completedImports, index, index+1)
	}
}
