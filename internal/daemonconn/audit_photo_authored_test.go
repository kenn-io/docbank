package daemonconn

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoAuthoredAuditConfirmationBoundary(t *testing.T) {
	for _, bad := range []string{"", "mask", "revision", "node", "missing"} {
		t.Run(bad, func(t *testing.T) {
			e := validAuditIngestObservation()
			e.Kind = "photo_authored"
			id := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			before := store.PhotoAuthoredSnapshot{FileID: id, NodeID: e.NodeID, Revision: 1}
			after := before
			after.Revision++
			after.Values.Confirmed = store.PhotoConfirmedCaption
			e.Attachment = &api.AuditAttachmentChange{Kind: "photo_authored", Identity: api.AuditAttachmentIdentity{FileID: id, NodeID: e.NodeID}, Before: &api.AuditAttachmentState{NodeID: e.NodeID, Photo: &before}, After: &api.AuditAttachmentState{NodeID: e.NodeID, Photo: &after}}
			switch bad {
			case "mask":
				after.Values.Confirmed = 128
			case "revision":
				after.Revision++
			case "node":
				e.Attachment.Identity.NodeID++
			case "missing":
				e.Attachment.After = nil
			}
			err := validateAuditEvent(e, e.NodeID)
			if bad == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
