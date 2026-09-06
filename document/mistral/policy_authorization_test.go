package mistral

import "testing"

func TestPolicyDoesNotAuthorizeUnprovedNonPDFFormats(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)

	for _, formatID := range []string{"docx", "pptx", "xlsx", "odt", "epub", "txt"} {
		t.Run(formatID, func(t *testing.T) {
			if expectedUnitBound(formatID) == UnitBoundLocalExact || localUnitCounters[formatID] != nil {
				t.Fatalf("unproved format %q is registered for local authority", formatID)
			}
			_, err := policy.Authorize(manifest, formatID)
			if err == nil {
				t.Fatalf("Policy.Authorize(%q) succeeded without provider-authentic unit evidence", formatID)
			}
		})
	}
}
