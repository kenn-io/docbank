package client

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestValidateAuditIngestObservation(t *testing.T) {
	event := validAuditIngestObservation()
	require.NoError(t, validateAuditEvent(event, event.NodeID))
}

func TestValidateAuditIngestObservationRejectsMalformedEvent(t *testing.T) {
	tests := map[string]func(*api.AuditEvent){
		"missing attachment": func(event *api.AuditEvent) {
			event.Attachment = nil
		},
		"wrong attachment": func(event *api.AuditEvent) {
			event.Attachment.Kind = "tag_assignment"
		},
		"identity mismatch": func(event *api.AuditEvent) {
			event.Attachment.Identity.ProvenanceID = auditTestHash("c")
		},
		"node mismatch": func(event *api.AuditEvent) {
			event.Attachment.After.NodeID++
		},
		"forbidden predecessor": func(event *api.AuditEvent) {
			predecessor := auditTestHash("d")
			event.Attachment.After.Supersedes = &predecessor
		},
		"zero prior revision": func(event *api.AuditEvent) {
			event.PriorNodeRevision = 0
			event.ResultingNodeRevision = 1
		},
		"revision jump": func(event *api.AuditEvent) {
			event.ResultingNodeRevision++
		},
		"missing prior version": func(event *api.AuditEvent) {
			event.PriorCurrentVersionID = nil
		},
		"changed version": func(event *api.AuditEvent) {
			changed := "22222222-2222-4222-8222-222222222222"
			event.ResultingCurrentVersionID = &changed
		},
		"source version transition": func(event *api.AuditEvent) {
			source := "33333333-3333-4333-8333-333333333333"
			event.SourceVersionID = &source
		},
		"target node transition": func(event *api.AuditEvent) {
			target := int64(8)
			event.TargetNodeID = &target
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			event := validAuditIngestObservation()
			mutate(&event)
			require.Error(t, validateAuditEvent(event, event.NodeID))
		})
	}
}

func validAuditIngestObservation() api.AuditEvent {
	versionID := "11111111-1111-4111-8111-111111111111"
	provenanceID := auditTestHash("b")
	return api.AuditEvent{
		ID:                        auditTestHash("a"),
		OperationID:               "44444444-4444-4444-8444-444444444444",
		OperationSequence:         1,
		NodeID:                    7,
		Kind:                      "ingest_observe",
		ScopeID:                   "55555555-5555-4555-8555-555555555555",
		RecordedAt:                "2026-09-10T00:00:00Z",
		Origin:                    "cli",
		PriorNodeRevision:         2,
		ResultingNodeRevision:     3,
		PriorCurrentVersionID:     &versionID,
		ResultingCurrentVersionID: &versionID,
		Attachment: &api.AuditAttachmentChange{
			Kind: "provenance",
			Identity: api.AuditAttachmentIdentity{
				ProvenanceID: provenanceID,
			},
			After: &api.AuditAttachmentState{
				NodeID:       7,
				ProvenanceID: provenanceID,
				IngestID:     "66666666-6666-4666-8666-666666666666",
			},
		},
	}
}

func auditTestHash(digit string) string {
	return strings.Repeat(digit, 64)
}
