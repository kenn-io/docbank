package daemonconn

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoAuthoredAuditConfirmationBoundary(t *testing.T) {
	for _, bad := range []string{"", "mask", "revision", "missing", "confirmed_fields"} {
		t.Run(bad, func(t *testing.T) {
			e := validAuditIngestObservation()
			e.Kind = "photo_authored"
			id := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			before := store.PhotoAuthoredSnapshot{FileID: id, NodeID: e.NodeID, Revision: 1}
			after := before
			after.Revision++
			after.Values.Confirmed = store.PhotoConfirmedCaption
			e.Attachment = &api.AuditAttachmentChange{Kind: "photo_authored", Identity: api.AuditAttachmentIdentity{FileID: id, NodeID: e.NodeID}, Before: &api.AuditAttachmentState{NodeID: e.NodeID, Photo: &before}, After: &api.AuditAttachmentState{NodeID: e.NodeID, Photo: &after, PhotoConfirmedFields: after.Values.Confirmed.Names()}}
			switch bad {
			case "mask":
				after.Values.Confirmed = 128
				e.Attachment.After.PhotoConfirmedFields = after.Values.Confirmed.Names()
			case "revision":
				after.Revision++
			case "missing":
				e.Attachment.After = nil
			case "confirmed_fields":
				e.Attachment.After.PhotoConfirmedFields = []string{"creator"}
			}
			err := validateAuditEvent(e, e.NodeID)
			switch bad {
			case "":
				require.NoError(t, err)
			default:
				require.Error(t, err)
			}
		})
	}
}
