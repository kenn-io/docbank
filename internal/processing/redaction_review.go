package processing

import (
	"fmt"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
)

func BuildRedactionReview(prep RedactionPreparation, masks []RedactionMask,
	observedRevision int64,
) (RedactionReview, error) {
	var result RedactionReview
	if observedRevision < 1 {
		return result, ErrRedactionInvalidRequest
	}
	if err := ValidateRedactionMasks(masks); err != nil {
		return result, err
	}
	if err := ValidateRedactionPreparation(prep); err != nil {
		return result, fmt.Errorf("%w: %w", ErrRedactionInvalidPreparation, err)
	}
	if err := production.ValidateNativePreparationDerivation(prep.NativePreparation); err != nil {
		return result, fmt.Errorf("%w: %w", ErrRedactionInvalidPreparation, err)
	}
	decisions := make([]redaction.Decision, len(masks))
	for i, mask := range masks {
		decisions[i] = redaction.Decision{ID: mask.ID.String(), MemberID: prep.Selection.Source.VersionID, Action: "redact", Selector: mask.Selector}
	}
	_, decisionsSHA, err := redaction.CanonicalDecisions(decisions)
	if err != nil {
		return result, err
	}
	recipe := pdfproduction.QualifiedRecipe()
	resolved, err := redaction.Resolve(prep.Map, "redact_selected", decisions, recipe)
	if err != nil {
		return result, err
	}
	preparationSHA, err := redactionPreparationDigest(prep)
	if err != nil {
		return result, err
	}
	result = RedactionReview{Contract: "native-pdf-redaction-review/v1",
		Selection:            NativePDFSelection{NodeID: prep.Selection.NodeID, Source: prep.Selection.Source},
		ObservedNodeRevision: observedRevision, PreparationSHA256: preparationSHA,
		DecisionsSHA256: decisionsSHA, Recipe: recipe, Resolved: resolved}
	result.SHA256, err = redactionReviewDigest(result)
	if err != nil {
		return RedactionReview{}, err
	}
	if _, exceeded, err := canonical.BoundedSize(result, MaxRedactionReviewBytes); err != nil {
		return RedactionReview{}, err
	} else if exceeded {
		return RedactionReview{}, ErrRedactionPreparationLimit
	}
	return result, nil
}

func redactionPreparationDigest(prep RedactionPreparation) (string, error) {
	projection := struct {
		production.NativePreparation

		Contract  string             `json:"contract"`
		Selection NativePDFSelection `json:"selection"`
	}{Contract: prep.Contract, Selection: NativePDFSelection{NodeID: prep.Selection.NodeID, Source: prep.Selection.Source}, NativePreparation: prep.NativePreparation}
	encoded, err := canonical.Marshal(projection)
	if err != nil {
		return "", err
	}
	return sha256HexBytes(encoded), nil
}

func redactionReviewDigest(review RedactionReview) (string, error) {
	projection := struct {
		Contract          string             `json:"contract"`
		Selection         NativePDFSelection `json:"selection"`
		PreparationSHA256 string             `json:"preparation_sha256"`
		DecisionsSHA256   string             `json:"decisions_sha256"`
		Recipe            redaction.Recipe   `json:"recipe"`
		Resolved          redaction.Resolved `json:"resolved"`
	}{review.Contract, review.Selection, review.PreparationSHA256, review.DecisionsSHA256, review.Recipe, review.Resolved}
	encoded, err := canonical.Marshal(projection)
	if err != nil {
		return "", err
	}
	return sha256HexBytes(encoded), nil
}

func ValidateRedactionReview(prep RedactionPreparation, masks []RedactionMask, review RedactionReview) error {
	expected, err := BuildRedactionReview(prep, masks, review.ObservedNodeRevision)
	if err != nil {
		return err
	}
	actual, err := redactionReviewDigest(review)
	if err != nil {
		return err
	}
	if actual != review.SHA256 || actual != expected.SHA256 {
		return ErrRedactionReviewChanged
	}
	return nil
}
