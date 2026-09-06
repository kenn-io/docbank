package document_test

import (
	"fmt"

	"go.kenn.io/docbank/document"
)

func ExampleBuildTranscriptEvidenceV1() {
	evidencePolicy, _ := document.NewEvidencePolicy(10_000)
	evidence, artifact, _ := document.BuildTranscriptEvidenceV1(document.SuppliedTranscript{
		Provider: "beeper",
		Text:     "The package arrived at dock seven",
	}, evidencePolicy)
	renditionPolicy, _ := document.NewRenditionPolicy(document.RenditionLimits{
		MaxDocumentChars: 10_000,
		MaxUnitRunes:     10_000,
		MaxSegmentRunes:  2_000,
	})
	rendition, _ := document.BuildRenditionV1(evidence, renditionPolicy)
	fmt.Println(rendition.LexicalSegments[0].Text)
	fmt.Println(artifact.Role, artifact.MediaType)
	// Output:
	// The package arrived at dock seven
	// provider_transcript application/json
}
