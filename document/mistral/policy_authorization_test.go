package mistral

import "testing"

func TestPolicyDoesNotAuthorizeUnprovedNonPDFFormats(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)

	for _, formatID := range []string{
		"docx", "pptx", "xlsx", "odt", "epub",
		"txt", "markdown", "csv", "json", "jsonl", "yaml",
		"go", "python", "javascript", "eml", "msg", "rst", "latex", "xml",
	} {
		t.Run(formatID, func(t *testing.T) {
			_, err := policy.Authorize(manifest, formatID)
			if err == nil {
				t.Fatalf("Policy.Authorize(%q) succeeded without provider-authentic unit evidence", formatID)
			}
		})
	}
	for _, formatID := range []string{
		"docx", "xlsx", "odt", "epub", "txt", "markdown", "csv", "jsonl", "yaml",
		"go", "python", "javascript", "msg", "rst", "latex", "xml",
	} {
		if expectedUnitBound(formatID) == UnitBoundLocalExact || localUnitCounters[formatID] != nil {
			t.Fatalf("unmeasured format %q is registered for local authority", formatID)
		}
	}
}
