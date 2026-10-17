package telemetry

import (
	"context"
	"sync"
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

// Imports keeps bounded, process-local attribution for browser imports.
type Imports struct {
	mu       sync.Mutex
	entries  map[string]importAttribution
	reporter *Capture
}

type importAttribution struct {
	surface string
}

func NewImports(reporter *Capture) *Imports {
	return &Imports{entries: make(map[string]importAttribution), reporter: reporter}
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
	_, exists := r.entries[key]
	if err == nil && fresh && surface != "" && !exists && len(r.entries) < 256 {
		r.entries[key] = importAttribution{surface: surface}
	}
	return err
}

// Result claims the first confirmed addition or retires a terminal no-add job.
func (r *Imports) Result(key string, added, terminal bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	deliver := r.claim(key, added, terminal)
	r.mu.Unlock()
	deliver()
}

// Observe claims stored progress under both locks, then delivers after release.
// Untracked or already reported imports skip the gate and storage read.
func (r *Imports) Observe(ctx context.Context, key string, mutate func(context.Context, func() error) error, read func() (int64, bool, error)) error {
	if !r.pending(key) {
		return nil
	}
	var deliver func()
	err := mutate(ctx, func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		count, terminal, err := read()
		if err == nil {
			deliver = r.claim(key, count > 0, terminal)
		}
		return err
	})
	if err == nil && deliver != nil {
		deliver()
	}
	return err
}

// pending reports whether key still awaits its first counted addition.
func (r *Imports) pending(key string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, exists := r.entries[key]
	return exists
}

func (r *Imports) claim(key string, added, terminal bool) func() {
	entry, exists := r.entries[key]
	added = exists && added
	if terminal || added {
		// A counted import has nothing left to attribute.
		delete(r.entries, key)
	}
	return func() {
		if added && entry.surface != "" {
			_ = r.reporter.Capture(EventDocumentAdded, map[string]any{"surface": entry.surface})
		}
	}
}
