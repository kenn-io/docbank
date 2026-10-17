package telemetry

import (
	"context"
	"sync"
	"time"
)

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

// Imports keeps bounded, process-local attribution. Admission holds the lock
// before publishing queued work so completion cannot overtake registration.
type Imports struct {
	mu       sync.Mutex
	entries  map[string]string
	reporter *Reporter
}

func NewImports(reporter *Reporter) *Imports {
	return &Imports{entries: make(map[string]string), reporter: reporter}
}

// Admit always runs the action, including when attribution is at capacity.
func (r *Imports) Admit(surface string, action func() (string, bool, error)) error {
	if r == nil {
		_, _, err := action()
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key, fresh, err := action()
	if err == nil && fresh && surface != "" && len(r.entries) < 256 {
		if _, exists := r.entries[key]; !exists {
			r.entries[key] = surface
		}
	}
	return err
}

// Result claims the first confirmed addition or retires a terminal no-add job.
func (r *Imports) Result(key string, added, terminal bool) {
	if r == nil || !added && !terminal {
		return
	}
	r.mu.Lock()
	surface := r.entries[key]
	delete(r.entries, key)
	r.mu.Unlock()
	if added && surface != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, _ = r.reporter.Report(ctx, EventDocumentAdded, map[string]any{"surface": surface})
	}
}
