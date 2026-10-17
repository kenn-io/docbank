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
	entries  map[string]importAttribution
	reporter *Reporter
}

type importAttribution struct {
	surface  string
	baseline int64
}

func NewImports(reporter *Reporter) *Imports {
	return &Imports{entries: make(map[string]importAttribution), reporter: reporter}
}

// Admit always runs the action, including when attribution is at capacity.
func (r *Imports) Admit(surface string, action func() (string, bool, error)) error {
	return r.AdmitCount(surface, func() (string, int64, bool, error) {
		key, fresh, err := action()
		return key, 0, fresh, err
	})
}

// AdmitCount saves the confirmed count before publishing resumed work.
func (r *Imports) AdmitCount(surface string, action func() (string, int64, bool, error)) error {
	if r == nil {
		_, _, _, err := action()
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key, baseline, fresh, err := action()
	if err == nil && fresh && surface != "" && len(r.entries) < 256 {
		if _, exists := r.entries[key]; !exists {
			r.entries[key] = importAttribution{surface: surface, baseline: baseline}
		}
	}
	return err
}

// Result claims the first confirmed addition or retires a terminal no-add job.
func (r *Imports) Result(key string, added, terminal bool) {
	r.result(key, terminal, func(importAttribution) bool { return added })
}

// ResultCount compares cumulative job progress with its admission baseline.
func (r *Imports) ResultCount(key string, count int64, terminal bool) {
	r.result(key, terminal, func(entry importAttribution) bool { return count > entry.baseline })
}

func (r *Imports) result(key string, terminal bool, confirmed func(importAttribution) bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	entry := r.entries[key]
	added := confirmed(entry)
	if added || terminal {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if added && entry.surface != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, _ = r.reporter.Report(ctx, EventDocumentAdded, map[string]any{"surface": entry.surface})
	}
}
