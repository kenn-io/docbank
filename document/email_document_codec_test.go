package document_test

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestEmailDocumentJSONRejectsUnboundedAndAmbiguousInput(t *testing.T) {
	for _, raw := range []string{`null`, `{"operation_id":"a","operation_id":"b"}`, `{"unknown":1}`, `{"destination_id":9007199254740992}`, `{"destination_id":1.0}`, `{"reuse":[` + strings.Repeat(`{},`, 1000) + `{}]}`, strings.Repeat(" ", 2<<20) + `{}`} {
		var request document.EmailDocumentPublicationRequest
		require.Error(t, document.UnmarshalEmailDocumentJSON([]byte(raw), &request))
	}
	var request document.EmailDocumentPublicationRequest
	require.NoError(t, document.UnmarshalEmailDocumentJSON([]byte(`{"operation_id":"synthetic","reuse":[]}`), &request))
	require.Equal(t, "synthetic", request.OperationID)
}

func TestEmailDocumentJSONCollectionCountsAttachmentObjects(t *testing.T) {
	identity := document.EmailDocumentIdentity{
		NodeID: 1, VersionID: "00000000-0000-4000-8000-000000000001",
		SHA256: strings.Repeat("a", 64), Size: 128,
	}
	receipt := document.EmailDocumentPublicationReceipt{
		OperationID: "synthetic-attachments", RequestDigest: strings.Repeat("b", 64),
		CreatedAt: "2026-01-01T00:00:00Z", InventoryState: "complete",
	}
	for i := range document.EmailDocumentMaxParts {
		child := identity
		child.NodeID += int64(i + 1)
		child.VersionID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+2)
		receipt.Relations = append(receipt.Relations, document.EmailDocumentRelation{
			OperationID: receipt.OperationID, Order: i + 1, Parent: identity,
			GenerationID: strings.Repeat("c", 64), AttachmentID: strings.Repeat("d", 64),
			PartPath: fmt.Sprintf("1.%d", i+1), SiblingOrder: i + 1,
			Filename: fmt.Sprintf("attachment-%d.txt", i+1), Outcome: "decoded", Child: &child,
		})
	}
	for _, count := range []int{64, document.EmailDocumentMaxParts} {
		t.Run(fmt.Sprintf("receipt_%d", count), func(t *testing.T) {
			want := receipt
			want.Relations = want.Relations[:count]
			raw, err := json.Marshal(want)
			require.NoError(t, err)
			var got document.EmailDocumentPublicationReceipt
			require.NoError(t, document.UnmarshalEmailDocumentJSON(raw, &got))
			require.NoError(t, document.ValidateEmailDocumentReceipt(got))
			require.Equal(t, want, got)
		})
	}
	page := document.EmailDocumentRelationPage{Total: int64(len(receipt.Relations))}
	for _, relation := range receipt.Relations[:250] {
		page.Items = append(page.Items, document.EmailDocumentRelationStatus{
			Relation: relation, State: "pending", Reason: "text_extraction",
		})
	}
	page.NextOperationID, page.NextOrder = receipt.OperationID, len(page.Items)
	raw, err := json.Marshal(page)
	require.NoError(t, err)
	var got document.EmailDocumentRelationPage
	require.NoError(t, document.UnmarshalEmailDocumentJSON(raw, &got))
	require.Equal(t, page, got)

	receipt.Relations = append(receipt.Relations, receipt.Relations[0])
	raw, err = json.Marshal(receipt)
	require.NoError(t, err)
	var oversized document.EmailDocumentPublicationReceipt
	require.ErrorContains(t, document.UnmarshalEmailDocumentJSON(raw, &oversized), "collection exceeds limit")
	require.Error(t, document.ValidateEmailDocumentReceipt(receipt))
}
