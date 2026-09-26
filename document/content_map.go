package document

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxContentMapSections         = 50
	MaxContentMapPins             = 1000
	MaxContentMapDescriptionBytes = 2 << 10
	MaxContentMapMetadataBytes    = 1 << 20
	MapPinVersionPinned           = "version-pinned"
	MapPinFollowCurrent           = "follow-current"
)

var ErrInvalidContentMap = errors.New("invalid content map")

// ContentMapPin names a stable document, an optional exact content version,
// and optionally an exact retained passage. A passage can never follow current.
type ContentMapPin struct {
	DocumentUID      string        `json:"document_uid"`
	Mode             string        `json:"mode"`
	ContentVersionID string        `json:"content_version_id,omitzero"`
	Passage          *PassageRefV1 `json:"passage,omitzero"`
}

// ContentMapPinIdentity identifies the curation instruction, independently of
// a source's current path or title. It rejects incomplete/invalid pin modes.
func ContentMapPinIdentity(pin ContentMapPin) (string, error) {
	if !passageUUID(pin.DocumentUID) {
		return "", fmt.Errorf("%w: document UID is invalid", ErrInvalidContentMap)
	}
	switch pin.Mode {
	case MapPinVersionPinned:
		if !passageUUID(pin.ContentVersionID) {
			return "", fmt.Errorf("%w: version-pinned item needs an exact content version", ErrInvalidContentMap)
		}
	case MapPinFollowCurrent:
		if pin.ContentVersionID != "" || pin.Passage != nil {
			return "", fmt.Errorf("%w: follow-current item cannot name a version or passage", ErrInvalidContentMap)
		}
	default:
		return "", fmt.Errorf("%w: unknown pin mode", ErrInvalidContentMap)
	}
	if pin.Passage != nil {
		if pin.Passage.DocumentUID != pin.DocumentUID || pin.Passage.ContentVersionID != pin.ContentVersionID ||
			ValidatePassageIdentityV1(*pin.Passage) != nil {
			return "", fmt.Errorf("%w: passage identity does not match its exact pin", ErrInvalidContentMap)
		}
		identity, _ := PassageIdentityV1(*pin.Passage)
		return "passage:" + identity, nil
	}
	return "document:" + pin.DocumentUID, nil
}

// ValidateMapPins rejects contradictory include/exclude identities.
func ValidateMapPins(include, exclude []string) error {
	seen := make(map[string]struct{}, len(include))
	for _, id := range include {
		if id == "" || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
			return fmt.Errorf("%w: pin identity is empty or invalid", ErrInvalidContentMap)
		}
		seen[id] = struct{}{}
	}
	for _, id := range exclude {
		if id == "" || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
			return fmt.Errorf("%w: pin identity is empty or invalid", ErrInvalidContentMap)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("%w: map item cannot be both included and excluded", ErrInvalidContentMap)
		}
	}
	return nil
}
