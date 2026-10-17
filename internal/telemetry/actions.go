package telemetry

// SearchType groups the executed search path without sending query text.
func SearchType(mode string) string {
	switch mode {
	case "semantic", "similar":
		return "semantic"
	case "hybrid":
		return "hybrid"
	default:
		return "text"
	}
}

// DocumentAdded reports an addition claimed by a committed import transaction.
func (c *Capture) DocumentAdded(surface string) {
	if c == nil {
		return
	}
	_ = c.Capture(EventDocumentAdded, map[string]any{"surface": surface})
}
