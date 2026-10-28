// Package answer classifies bounded evidence without synthesizing an answer.
package answer

type Status string

const (
	Answered           Status = "ANSWERED"
	NoMatch            Status = "NO_MATCH"
	IncompleteCoverage Status = "INCOMPLETE_COVERAGE"
)

type Passage struct {
	NodeID           int64    `json:"node_id"`
	ContentVersionID string   `json:"content_version_id"`
	Path             string   `json:"path"`
	Rank             int      `json:"rank"`
	Text             string   `json:"text"`
	TextBytes        int      `json:"text_bytes"`
	EvidenceIDs      []string `json:"evidence_ids"`
	Relevance        *float64 `json:"relevance,omitempty"`
}
type Lead struct {
	NodeID           int64    `json:"node_id"`
	ContentVersionID string   `json:"content_version_id"`
	Path             string   `json:"path"`
	Rank             int      `json:"rank"`
	EvidenceIDs      []string `json:"evidence_ids"`
	Relevance        *float64 `json:"relevance,omitempty"`
}
type Evidence struct {
	Passages []Passage
	Leads    []Lead
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

// Classify only considers evidence retained after output budgeting.
// truncated includes any retrieval or output omission that could hide evidence.
func Classify(e Evidence, complete, truncated bool) Status {
	if len(e.Passages) > 0 {
		return Answered
	}
	if len(e.Leads) > 0 || !complete || truncated {
		return IncompleteCoverage
	}
	return NoMatch
}
