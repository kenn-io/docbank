package processing

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"uuid"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

func ValidateRedactionMasks(masks []RedactionMask) error {
	if len(masks) < 1 || len(masks) > 256 {
		return ErrRedactionInvalidRequest
	}
	seen := make(map[uuid.UUID]bool, len(masks))
	boxes, pages := 0, 0
	for _, mask := range masks {
		if seen[mask.ID] {
			return ErrRedactionInvalidRequest
		}
		seen[mask.ID] = true
		switch mask.Selector.Kind {
		case "text", "rectangle", "page":
		default:
			return ErrRedactionInvalidRequest
		}
		// Validate the same shape accepted by the resolver; member identity does not affect shape.
		decision := redaction.Decision{ID: mask.ID.String(), MemberID: mask.ID.String(), Action: "redact", Selector: mask.Selector}
		if err := redaction.ValidateDecision(decision); err != nil {
			return fmt.Errorf("%w: %w", ErrRedactionInvalidRequest, err)
		}
		boxes += len(mask.Selector.Boxes)
		pages += len(mask.Selector.Pages)
		if boxes > 1024 || pages > 1024 {
			return ErrRedactionInvalidRequest
		}
	}
	if _, exceeded, err := canonical.BoundedSize(masks, MaxRedactionMaskBytes); err != nil {
		return err
	} else if exceeded {
		return ErrRedactionInvalidRequest
	}
	return nil
}

// DecodeRedactionMasks checks spelling before UUID decoding can normalize it.
func DecodeRedactionMasks(data []byte) ([]RedactionMask, error) {
	var raw []struct {
		ID       string             `json:"id"`
		Selector redaction.Selector `json:"selector"`
	}
	if int64(len(data)) > MaxRedactionRequestBytes {
		return nil, ErrRedactionInvalidRequest
	}
	if err := json.Unmarshal(data, &raw, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRedactionInvalidRequest, err)
	}
	if len(raw) < 1 || len(raw) > 256 {
		return nil, ErrRedactionInvalidRequest
	}
	masks := make([]RedactionMask, len(raw))
	for i, value := range raw {
		if err := canonical.ValidateUUIDv4(value.ID); err != nil {
			return nil, ErrRedactionInvalidRequest
		}
		id, err := uuid.Parse(value.ID)
		if err != nil {
			return nil, ErrRedactionInvalidRequest
		}
		masks[i] = RedactionMask{ID: id, Selector: value.Selector}
	}
	return masks, ValidateRedactionMasks(masks)
}

// redactionPreparationTransport accepts the schema link added to HTTP responses.
// It is transport metadata, not part of the preparation's evidence identity.
type redactionPreparationTransport struct {
	RedactionPreparation

	Schema string `json:"$schema,omitzero"`
}

func DecodeRedactionResolveRequest(data []byte) (RedactionResolveRequest, error) {
	var raw struct {
		Preparation redactionPreparationTransport `json:"preparation"`
		Masks       jsontext.Value                `json:"masks"`
	}
	if int64(len(data)) > MaxRedactionRequestBytes {
		return RedactionResolveRequest{}, ErrRedactionInvalidRequest
	}
	if err := json.Unmarshal(data, &raw, json.RejectUnknownMembers(true)); err != nil {
		return RedactionResolveRequest{}, fmt.Errorf("%w: %w", ErrRedactionInvalidRequest, err)
	}
	masks, err := DecodeRedactionMasks(raw.Masks)
	return RedactionResolveRequest{Preparation: raw.Preparation.RedactionPreparation, Masks: masks}, err
}

func DecodeRedactionRenderRequest(data []byte) (RedactionRenderRequest, error) {
	var raw struct {
		AllocationID jsontext.Value                `json:"allocation_id,omitzero"`
		Preparation  redactionPreparationTransport `json:"preparation"`
		Masks        jsontext.Value                `json:"masks"`
		ReviewSHA256 string                        `json:"review_sha256"`
	}
	if int64(len(data)) > MaxRedactionRequestBytes {
		return RedactionRenderRequest{}, ErrRedactionInvalidRequest
	}
	if err := json.Unmarshal(data, &raw, json.RejectUnknownMembers(true)); err != nil {
		return RedactionRenderRequest{}, fmt.Errorf("%w: %w", ErrRedactionInvalidRequest, err)
	}
	if !canonical.IsSHA256Hex(raw.ReviewSHA256) {
		return RedactionRenderRequest{}, ErrRedactionInvalidRequest
	}
	var allocation uuid.UUID
	if len(raw.AllocationID) > 0 {
		var id string
		if err := json.Unmarshal(raw.AllocationID, &id); err != nil || canonical.ValidateUUIDv4(id) != nil {
			return RedactionRenderRequest{}, ErrRedactionInvalidRequest
		}
		allocation = uuid.MustParse(id)
	}
	masks, err := DecodeRedactionMasks(raw.Masks)
	return RedactionRenderRequest{Preparation: raw.Preparation.RedactionPreparation, Masks: masks, ReviewSHA256: raw.ReviewSHA256, AllocationID: allocation}, err
}

func DecodeNumberedReservationRequest(data []byte) (NumberedReservationRequest, error) {
	var raw struct {
		Preparation    redactionPreparationTransport `json:"preparation"`
		Masks          jsontext.Value                `json:"masks"`
		ReviewSHA256   string                        `json:"review_sha256"`
		NamespaceID    string                        `json:"namespace_id"`
		StartAt        int64                         `json:"start_at"`
		RetainOriginal bool                          `json:"retain_original"`
	}
	if int64(len(data)) > MaxRedactionRequestBytes {
		return NumberedReservationRequest{}, ErrRedactionInvalidRequest
	}
	if err := json.Unmarshal(data, &raw, json.RejectUnknownMembers(true)); err != nil {
		return NumberedReservationRequest{}, ErrRedactionInvalidRequest
	}
	if !raw.RetainOriginal || raw.StartAt < 1 || canonical.ValidateUUIDv4(raw.NamespaceID) != nil || !canonical.IsSHA256Hex(raw.ReviewSHA256) {
		return NumberedReservationRequest{}, ErrRedactionInvalidRequest
	}
	masks, err := DecodeRedactionMasks(raw.Masks)
	return NumberedReservationRequest{Preparation: raw.Preparation.RedactionPreparation, Masks: masks,
		ReviewSHA256: raw.ReviewSHA256, NamespaceID: uuid.MustParse(raw.NamespaceID), StartAt: raw.StartAt, RetainOriginal: true}, err
}
