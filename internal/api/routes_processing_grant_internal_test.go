package api

import (
	"testing"
	"time"
)

func TestProcessingStartSourceGrantCapturesAuthorizedScope(t *testing.T) {
	sourceID := "11111111-1111-4111-8111-111111111111"
	expiry := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	principal := Principal{SubjectID: "subject:synthetic", CredentialKind: "machine",
		Audience: "docbank:test", GrantRevision: 7, ExpiresAt: expiry,
		SourceIDs: []string{sourceID}, Operations: []Operation{OperationProcessing}}
	decision := OperationAuthorization{SourceIDs: []string{sourceID}, GrantRevision: 7}
	binding, scoped, err := processingStartSourceGrant(ContextWithPrincipal(t.Context(), principal), decision, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if !scoped || binding.SubjectID != principal.SubjectID || binding.CredentialKind != principal.CredentialKind ||
		binding.Audience != principal.Audience || binding.GrantRevision != principal.GrantRevision ||
		!binding.ExpiresAt.Equal(expiry) || binding.SourceID != sourceID {
		t.Fatalf("incorrect source grant binding: %#v", binding)
	}
	if local, scoped, err := processingStartSourceGrant(ContextWithPrincipal(t.Context(), LocalAdminPrincipal()),
		OperationAuthorization{SourceIDs: []string{sourceID}, Unrestricted: true}, sourceID); err != nil || scoped {
		t.Fatalf("local binding = %#v, scoped = %t, %v", local, scoped, err)
	}
	decision.GrantRevision++
	if _, _, err := processingStartSourceGrant(ContextWithPrincipal(t.Context(), principal), decision, sourceID); err == nil {
		t.Fatal("mismatched grant revision was accepted")
	}
}
