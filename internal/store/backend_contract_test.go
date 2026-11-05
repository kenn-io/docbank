package store

import (
	"flag"
	"fmt"
	"testing"
)

// This registry is the executable evidence inventory, not a production capability
// switch. Each scenario runs once on each selected backend. Backend-specific
// cases keep their own fixtures; a copied catalog never qualifies its producer.
type backendScenario struct {
	name     string
	sqlite   func(*testing.T)
	postgres func(*testing.T)
}
type backendSuite struct {
	name      string
	postgres  string
	scope     string
	scenarios []backendScenario
}

var backendSuites = []backendSuite{
	{name: "photo-editing", postgres: "unqualified", scope: "People/face publication, best-shot/representative choices, hidden-session management and photo review remain unqualified. Photo query selection is covered by the search boundary."},
