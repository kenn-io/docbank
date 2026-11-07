package daemonconn

import (
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func validatePublishedNoteIdentity(identity *api.PublishedNoteIdentity, selector *api.PublishedNoteSelector, nodeID int64, version, vault string, size int64) error {
	if identity == nil {
		if selector != nil {
			return errors.New("published note response identity is missing")
		}
		return nil
	}
	if identity.NodeID != nodeID || identity.ContentVersionID != version || (vault != "" && identity.VaultID != vault) || (size >= 0 && identity.Size != size) {
		return errors.New("published note response identity disagrees with document")
	}
	if selector != nil && selector.Publication != "" && identity.Publication != selector.Publication {
		return errors.New("published note response escaped its publication")
	}
	if err := store.ValidatePublishedNoteSource(store.NotePublicationSource{Identity: store.PublishedNoteIdentity(*identity), Presence: "present", Availability: "retained"}); err != nil {
		return errors.New("published note response identity is malformed")
	}
	return nil
}
