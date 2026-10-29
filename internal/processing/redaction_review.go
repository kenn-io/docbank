package processing

import (
	"fmt"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
)

// BuildRedactionReview binds a decision to an explicit qualified native recipe.
func BuildRedactionReview(prep RedactionPreparation, masks []RedactionMask,
	observedRevision int64, recipe redaction.Recipe,
) (RedactionReview, error) {
	if err := qualifyNativeRecipe(recipe); err != nil {
		return RedactionReview{}, err
	}
	review, decisions, err := prepareRedactionReview(prep, masks, observedRevision)
	if err != nil {
		return RedactionReview{}, err
	}
	return resolveRedactionReview(prep.Map, review, decisions, recipe)
}

// MatchRedactionReview resolves the recorded digest against our finite recipe
// catalog before I/O. It never selects a format by inspecting a PDF.
func MatchRedactionReview(prep RedactionPreparation, masks []RedactionMask,
	observedRevision int64, digest string,
) (RedactionReview, error) {
	base, decisions, err := prepareRedactionReview(prep, masks, observedRevision)
	if err != nil {
		return RedactionReview{}, err
	}
	recipes, err := pdfproduction.QualifiedRecipesForDPI(300)
	if err != nil {
		return RedactionReview{}, err
	}
	var match RedactionReview
	count := 0
	for _, recipe := range recipes {
		review, err := resolveRedactionReview(prep.Map, base, decisions, recipe)
		if err != nil {
			return RedactionReview{}, err
		}
		if review.SHA256 == digest {
			match = review
			count++
		}
	}
	if count != 1 {
		return RedactionReview{}, ErrRedactionReviewChanged
	}
	return match, nil
}

func qualifyNativeRecipe(recipe redaction.Recipe) error {
	if recipe.DPI != 300 {
		return pdfproduction.ErrVerifierVersion
	}
	return pdfproduction.QualifyRecipe(recipe)
}

func prepareRedactionReview(prep RedactionPreparation, masks []RedactionMask,
	observedRevision int64,
) (RedactionReview, []redaction.Decision, error) {
	var result RedactionReview
	if observedRevision < 1 {
		return result, nil, ErrRedactionInvalidRequest
	}
	if err := ValidateRedactionMasks(masks); err != nil {
		return result, nil, err
	}
	if err := ValidateRedactionPreparation(prep); err != nil {
		return result, nil, fmt.Errorf("%w: %w", ErrRedactionInvalidPreparation, err)
	}
	if err := production.ValidateNativePreparationDerivation(prep.NativePreparation); err != nil {
		return result, nil, fmt.Errorf("%w: %w", ErrRedactionInvalidPreparation, err)
	}
	decisions := make([]redaction.Decision, len(masks))
	for i, mask := range masks {
		decisions[i] = redaction.Decision{ID: mask.ID.String(), MemberID: prep.Selection.Source.VersionID, Action: "redact", Selector: mask.Selector}
	}
	_, decisionsSHA, err := redaction.CanonicalDecisions(decisions)
	if err != nil {
		return result, nil, err
	}
	preparationSHA, err := redactionPreparationDigest(prep)
	if err != nil {
		return result, nil, err
	}
	result = RedactionReview{Contract: "native-pdf-redaction-review/v1",
		Selection:            NativePDFSelection{NodeID: prep.Selection.NodeID, Source: prep.Selection.Source},
		ObservedNodeRevision: observedRevision, PreparationSHA256: preparationSHA, DecisionsSHA256: decisionsSHA}
	return result, decisions, nil
}

func resolveRedactionReview(textMap redaction.TextMap, result RedactionReview,
	decisions []redaction.Decision, recipe redaction.Recipe,
) (RedactionReview, error) {
	resolved, err := redaction.Resolve(textMap, "redact_selected", decisions, recipe)
	if err != nil {
		return RedactionReview{}, err
	}
	result.Recipe, result.Resolved = recipe, resolved
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
	if err := qualifyNativeRecipe(review.Recipe); err != nil {
		return err
	}
	expected, err := BuildRedactionReview(prep, masks, review.ObservedNodeRevision, review.Recipe)
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
