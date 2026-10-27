package docbank

import (
	"context"

	"go.kenn.io/docbank/internal/misslog"
)

var (
	ErrSearchReceiptUnavailable  = misslog.ErrUnavailable
	ErrSearchFeedbackInvalid     = misslog.ErrInvalid
	ErrSearchFeedbackPersistence = misslog.ErrPersistence
)

// SearchFeedbackIdentity names an expected document and optional exact version.
type SearchFeedbackIdentity struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id,omitempty"`
}

// SearchFeedback labels a receipt. Verdict is wrong, missing, low_confidence,
// or none_in_vault. Expected is optional except that none_in_vault forbids it.
type SearchFeedback struct {
	Verdict  string                   `json:"verdict"`
	Expected []SearchFeedbackIdentity `json:"expected,omitempty"`
	Note     string                   `json:"note,omitempty"`
}

// RecordSearchFeedback is an explicit owner write, outside CallReadTool.
// It labels stored receipts even while recording is disabled. Cached receipts
// expire after 15 minutes and cannot be recovered after eviction or restart.
func (v *Vault) RecordSearchFeedback(ctx context.Context, id string, feedback SearchFeedback) error {
	if err := v.begin(); err != nil {
		return err
	}
	defer v.lifecycle.RUnlock()
	if len(feedback.Expected) > 20 {
		return ErrSearchFeedbackInvalid
	}
	expected := make([]misslog.Identity, len(feedback.Expected))
	for i, id := range feedback.Expected {
		expected[i] = misslog.Identity{NodeID: id.NodeID, ContentVersionID: id.ContentVersionID}
	}
	return v.missLog.Feedback(ctx, id, "", misslog.Feedback{Verdict: feedback.Verdict, Expected: expected, Note: feedback.Note}, true)
}
