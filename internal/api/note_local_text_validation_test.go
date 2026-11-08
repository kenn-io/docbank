package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
)

func TestValidateNoteLocalTextPlan(t *testing.T) {
	t.Parallel()
	name, fixed, err := processing.NewNotePlaintextProfile()
	require.NoError(t, err)
	_, fingerprints, err := document.CanonicalProfile(fixed.Profile)
	require.NoError(t, err)
	inputs := []string{"original_file"}
	retained := []string{"normalized_evidence", "sanitized_markdown"}
	provider := fixed.Profile.Rendition.Descriptor.ID
	valid := NoteLocalTextPlan{Plan: ProcessingPlan{
		Fingerprint: strings.Repeat("a", 64), Selector: ProcessingSelector{Profile: name}, ProfileFingerprint: fingerprints.Profile,
		Flow: []ProcessingFlowHop{{Capability: "rendition", ProviderID: provider, TrustBoundary: string(document.RenditionTrustLocalProcess), InputClasses: inputs,
			RuntimeDisclosure: ProcessingRuntimeDisclosure{ImmediateProcessor: provider, UltimateProcessor: provider, Endpoint: "in-process",
				Deployment: fixed.Profile.Rendition.DeploymentFingerprint, MetadataClasses: []string{"byte_length", "content_hash", "detected_media_type", "synthetic_filename"}, RetainedArtifactRoles: retained}}},
		DisclosedClasses: inputs, RetainedClasses: retained, Estimate: ProcessingEstimate{ProviderCalls: 1}, ConsentRequired: true, ConsentState: "required",
	}, ConsentRequest: document.ProcessingConsentRequest{Principal: "daemon:operator", Scope: "document-processing", ProfileFingerprint: fingerprints.Profile,
		DisclosureFingerprint: fixed.Profile.Rendition.DisclosureFingerprint, InputClasses: inputs, RetainedArtifactClasses: retained}}
	require.NoError(t, ValidateNoteLocalTextPlan(valid))
	for name, mutate := range map[string]func(*NoteLocalTextPlan){
		"extra flow": func(p *NoteLocalTextPlan) {
			p.Plan.Flow = append(p.Plan.Flow, ProcessingFlowHop{Capability: "embedding"})
		},
		"remote endpoint":      func(p *NoteLocalTextPlan) { p.Plan.Flow[0].RuntimeDisclosure.Endpoint = "https://example.invalid" },
		"wrong profile":        func(p *NoteLocalTextPlan) { p.ConsentRequest.ProfileFingerprint = strings.Repeat("b", 64) },
		"filename":             func(p *NoteLocalTextPlan) { p.Plan.Flow[0].DiscloseFilename = true },
		"broader scope":        func(p *NoteLocalTextPlan) { p.ConsentRequest.Scope = "all" },
		"inconsistent consent": func(p *NoteLocalTextPlan) { p.Plan.ConsentState = "active" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := valid
			copy.Plan.Flow = append([]ProcessingFlowHop(nil), valid.Plan.Flow...)
			mutate(&copy)
			require.Error(t, ValidateNoteLocalTextPlan(copy))
		})
	}
}
