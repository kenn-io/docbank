// Package answer classifies bounded evidence without synthesizing an answer.
package answer

import "context"

type Status string

const (
	Answered           Status = "ANSWERED"
	NoMatch            Status = "NO_MATCH"
	IncompleteCoverage Status = "INCOMPLETE_COVERAGE"
	OutdatedOnly       Status = "OUTDATED_ONLY"
	Conflicting        Status = "CONFLICTING"
)

type Supersession struct {
	NodeID           int64
	ContentVersionID string
}
type Passage struct {
	NodeID           int64         `json:"node_id"`
	ContentVersionID string        `json:"content_version_id"`
	Path             string        `json:"path"`
	Rank             int           `json:"rank"`
	Text             string        `json:"text"`
	TextBytes        int           `json:"text_bytes"`
	EvidenceIDs      []string      `json:"evidence_ids"`
	Relevance        *float64      `json:"relevance,omitempty"`
	Outdated         *Supersession `json:"-"`
	ConflictGroup    string        `json:"-"`
	Dropped          string        `json:"-"`
}
type Lead struct {
	NodeID           int64    `json:"node_id"`
	ContentVersionID string   `json:"content_version_id"`
	Path             string   `json:"path"`
	Rank             int      `json:"rank"`
	EvidenceIDs      []string `json:"evidence_ids"`
	Relevance        *float64 `json:"relevance,omitempty"`
	Dropped          string   `json:"-"`
}
type Evidence struct {
	Passages []Passage
	Leads    []Lead
}
type Question struct{ Text string }
type GateOutcome struct {
	Outcome string
	Dropped int
}
type Gate interface {
	ID() string
	Version() string
	Apply(ctx context.Context, question Question, evidence Evidence) (Evidence, GateOutcome, error)
}

// Budget keeps a prefix of whole passages, counting UTF-8 bytes.
func Budget(passages []Passage, maxBytes int) ([]Passage, int, int) {
	n := 0
	for i, p := range passages {
		if len(p.Text) > maxBytes-n {
			return passages[:i], n, len(passages) - i
		}
		n += len(p.Text)
	}
	return passages, n, 0
}

// Classify only considers evidence retained after gates and output budgeting.
// truncated includes any retrieval or output omission that could hide evidence.
func Classify(e Evidence, complete, truncated bool) Status {
	current, outdated := 0, 0
	groups := map[string]int{}
	for _, p := range e.Passages {
		if p.Dropped != "" {
			continue
		}
		if p.Outdated != nil {
			outdated++
			continue
		}
		current++
		if p.ConflictGroup != "" {
			groups[p.ConflictGroup]++
		}
	}
	if current > 0 {
		for _, count := range groups {
			if count >= 2 {
				return Conflicting
			}
		}
		return Answered
	}
	for _, l := range e.Leads {
		if l.Dropped == "" {
			return IncompleteCoverage
		}
	}
	if !complete || truncated {
		return IncompleteCoverage
	}
	if outdated > 0 {
		return OutdatedOnly
	}
	return NoMatch
}

// GateReceipt records the result of one configured gate.
type GateReceipt struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Outcome string `json:"outcome"`
	Dropped int    `json:"dropped"`
}

// ApplyGates executes gates in the supplied order and removes dropped evidence.
// Errors are returned to the caller without degrading the classification.
func ApplyGates(ctx context.Context, question Question, evidence Evidence, gates []Gate) (Evidence, []GateReceipt, error) {
	receipts := make([]GateReceipt, 0, len(gates))
	for _, gate := range gates {
		next, outcome, err := gate.Apply(ctx, question, evidence)
		if err != nil {
			return Evidence{}, nil, err
		}
		evidence = next
		receipts = append(receipts, GateReceipt{ID: gate.ID(), Version: gate.Version(), Outcome: outcome.Outcome, Dropped: outcome.Dropped})
	}
	passages := make([]Passage, 0, len(evidence.Passages))
	for _, p := range evidence.Passages {
		if p.Dropped == "" {
			passages = append(passages, p)
		}
	}
	leads := make([]Lead, 0, len(evidence.Leads))
	for _, l := range evidence.Leads {
		if l.Dropped == "" {
			leads = append(leads, l)
		}
	}
	return Evidence{Passages: passages, Leads: leads}, receipts, nil
}
