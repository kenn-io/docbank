package misslog

import (
	"context"
	"crypto/hmac"
	"encoding/json/v2"
	"errors"
	"math"
	"slices"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/store"
)

const maxPendingBytes = 8 << 20

var (
	ErrUnavailable = errors.New("search receipt or feedback capability unavailable")
	ErrInvalid     = errors.New("invalid search feedback")
	ErrPersistence = errors.New("miss log operation failed")
)

// Identity names a document, optionally at an exact retained content version.
type Identity struct {
	NodeID           int64  `json:"node_id" minimum:"1"`
	ContentVersionID string `json:"content_version_id,omitempty" maxLength:"36"`
}

// Hit contains bounded search evidence, never an excerpt. A live folder may
// have no content version or score.
type Hit struct {
	Rank             int      `json:"rank" minimum:"1"`
	NodeID           int64    `json:"node_id" minimum:"1"`
	ContentVersionID string   `json:"content_version_id,omitempty" maxLength:"36"`
	Score            *float64 `json:"score,omitempty"`
	Path             string   `json:"path,omitempty" maxLength:"16384"`
	EvidenceIDs      []string `json:"evidence_ids,omitempty" maxItems:"64" maxLength:"1024"`
}

// Feedback is a correction, not a confidence estimate derived from rank scores.
// Positive verdicts may omit expected identities until the operator knows them.
type Feedback struct {
	Verdict  string     `json:"verdict" enum:"wrong,missing,low_confidence,none_in_vault"`
	Expected []Identity `json:"expected,omitempty" maxItems:"20"`
	Note     string     `json:"note,omitempty" maxLength:"1000"`
}

func ValidateFeedback(f Feedback) error {
	if !slices.Contains([]string{"wrong", "missing", "low_confidence", "none_in_vault"}, f.Verdict) || !utf8.ValidString(f.Note) || len(f.Note) > 4000 || utf8.RuneCountInString(f.Note) > 1000 || len(f.Expected) > 20 || (f.Verdict == "none_in_vault" && len(f.Expected) > 0) {
		return ErrInvalid
	}
	seen := map[int64]bool{}
	for _, id := range f.Expected {
		if id.NodeID < 1 || len(id.ContentVersionID) > 36 || seen[id.NodeID] {
			return ErrInvalid
		}
		seen[id.NodeID] = true
	}
	return nil
}
func validHits(hits []Hit) bool {
	if len(hits) > 10 {
		return false
	}
	for _, h := range hits {
		if h.Rank < 1 || h.NodeID < 1 || len(h.ContentVersionID) > 36 || len(h.Path) > 16384 || len(h.EvidenceIDs) > 64 || (h.Score != nil && (math.IsNaN(*h.Score) || math.IsInf(*h.Score, 0))) {
			return false
		}
		for _, id := range h.EvidenceIDs {
			if len(id) > 1024 {
				return false
			}
		}
	}
	return true
}

// Feedback atomically persists a cached receipt or replaces a stored label.
// The operator may label by ID. Other callers must present the separate ticket.
func (s *Service) Feedback(ctx context.Context, id, capability string, feedback Feedback, operator bool) error {
	if ValidateFeedback(feedback) != nil {
		return ErrInvalid
	}
	if s == nil || s.store == nil || len(id) != 48 {
		return ErrUnavailable
	}
	if !operator && (!s.config.Enabled || len(capability) != 64 || !hmac.Equal([]byte(capability), []byte(s.capability(id)))) {
		return ErrUnavailable
	}
	err := s.mutate(ctx, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.expirePending()
		for _, identity := range feedback.Expected {
			node, err := s.store.NodeByID(ctx, identity.NodeID)
			if errors.Is(err, store.ErrNotFound) {
				return ErrInvalid
			}
			if err != nil {
				return ErrPersistence
			}
			if node.Kind != "file" || node.TrashedAt != nil {
				return ErrInvalid
			}
			if identity.ContentVersionID != "" {
				version, err := s.store.ContentVersionByID(ctx, identity.ContentVersionID)
				if errors.Is(err, store.ErrNotFound) {
					return ErrInvalid
				}
				if err != nil {
					return ErrPersistence
				}
				if version.NodeID != identity.NodeID {
					return ErrInvalid
				}
			}
		}
		index := -1
		var receipt Receipt
		for i, p := range s.pending {
			if p.id == id {
				index = i
				if json.Unmarshal(p.encoded, &receipt) != nil {
					return ErrPersistence
				}
				break
			}
		}
		if index < 0 {
			row, err := s.store.MissLogRecordByID(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return ErrUnavailable
			}
			if err != nil {
				return ErrPersistence
			}
			if json.Unmarshal([]byte(row.Receipt), &receipt) != nil || receipt.VaultUID != s.store.VaultID() {
				return ErrPersistence
			}
		}
		receipt.Feedback = &feedback
		trigger := "correction"
		if feedback.Verdict == "low_confidence" {
			trigger = "low_confidence"
		}
		if !slices.Contains(receipt.Triggers, trigger) {
			receipt.Triggers = append(receipt.Triggers, trigger)
		}
		encoded, err := json.Marshal(receipt)
		if err != nil || len(encoded) > maxPendingBytes {
			return ErrInvalid
		}
		record := store.MissLogRecord{ID: id, CreatedAt: receipt.CreatedAt, Receipt: string(encoded)}

		err = s.store.SaveMissLogFeedback(ctx, record, s.now(), s.config.RetentionDays, s.config.MaxRecords, index >= 0)
		if err != nil {
			return ErrPersistence
		}
		if index >= 0 {
			s.removePending(index)
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrUnavailable) {
		return ErrPersistence
	}
	return err
}

// Delete removes persisted and pending receipts under the same mutation and
// service locks as feedback, so a deleted cached receipt cannot reappear.
// An empty ID deletes before cutoff, or purges everything when cutoff is zero.
func (s *Service) Delete(ctx context.Context, id string, cutoff time.Time) error {
	if s == nil || s.store == nil {
		return ErrPersistence
	}
	return s.mutate(ctx, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		var err error
		switch {
		case id != "":
			err = s.store.DeleteMissLogRecord(ctx, id)
		case !cutoff.IsZero():
			err = s.store.DeleteMissLogRecordsBefore(ctx, cutoff)
		default:
			err = s.store.PurgeMissLogRecords(ctx)
		}
		if err != nil {
			return ErrPersistence
		}
		for i := len(s.pending) - 1; i >= 0; i-- {
			r := s.pending[i]
			if (id != "" && r.id == id) || (id == "" && (cutoff.IsZero() || r.createdAt.Before(cutoff))) {
				s.removePending(i)
			}
		}
		return nil
	})
}
