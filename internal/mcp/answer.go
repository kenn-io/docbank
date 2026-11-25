package mcp

type AnswerStatus string

const (
	AnswerStatusAnswered           AnswerStatus = "ANSWERED"
	AnswerStatusNoMatch            AnswerStatus = "NO_MATCH"
	AnswerStatusIncompleteCoverage AnswerStatus = "INCOMPLETE_COVERAGE"
)

type AnswerPassage struct {
	NodeID           int64    `json:"node_id"`
	ContentVersionID string   `json:"content_version_id"`
	Path             string   `json:"path"`
	Rank             int      `json:"rank"`
	Text             string   `json:"text"`
	TextBytes        int      `json:"text_bytes"`
	EvidenceIDs      []string `json:"evidence_ids"`
	Relevance        *float64 `json:"relevance,omitempty"`
}
type AnswerLead struct {
	NodeID           int64    `json:"node_id"`
	ContentVersionID string   `json:"content_version_id"`
	Path             string   `json:"path"`
	Rank             int      `json:"rank"`
	EvidenceIDs      []string `json:"evidence_ids"`
	Relevance        *float64 `json:"relevance,omitempty"`
}
type AnswerEvidence struct {
	Passages []AnswerPassage
	Leads    []AnswerLead
}

// BudgetAnswerPassages keeps a prefix of whole passages, counting UTF-8 bytes.
func BudgetAnswerPassages(passages []AnswerPassage, maxBytes int) ([]AnswerPassage, int, int) {
	n := 0
	for i, p := range passages {
		if len(p.Text) > maxBytes-n {
			return passages[:i], n, len(passages) - i
		}
		n += len(p.Text)
	}
	return passages, n, 0
}

// ClassifyAnswer only considers evidence retained after output budgeting.
// truncated includes any retrieval or output omission that could hide evidence.
func ClassifyAnswer(e AnswerEvidence, complete, truncated bool) AnswerStatus {
	if len(e.Passages) > 0 {
		return AnswerStatusAnswered
	}
	if len(e.Leads) > 0 || !complete || truncated {
		return AnswerStatusIncompleteCoverage
	}
	return AnswerStatusNoMatch
}
