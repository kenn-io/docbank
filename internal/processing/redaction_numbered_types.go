package processing

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"unicode/utf8"
	"uuid"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/pdfstamp"
	"go.kenn.io/docbank/internal/store"
)

const NumberedPDFContract = "native-pdf-numbered/v1"
const MaxNumberedRecipeBytes = 64 << 10
const MaxNumberedReservationBytes = 128 << 10

var ErrNumberedAllocationNotFound = errors.New("numbered allocation not found")
var ErrNumberedNamespaceNotFound = errors.New("bates namespace not found")

type NumberedNamespace struct {
	ID      uuid.UUID `json:"id"`
	Prefix  string    `json:"prefix"`
	Suffix  string    `json:"suffix"`
	Padding int       `json:"padding"`
	Start   int64     `json:"start"`
}

type NumberedRecipe struct {
	Contract                 string                       `json:"contract"`
	VaultUID                 uuid.UUID                    `json:"vault_uid"`
	Selection                NativePDFSelection           `json:"selection"`
	PreparationSHA256        string                       `json:"preparation_sha256"`
	DecisionsSHA256          string                       `json:"decisions_sha256"`
	ReviewSHA256             string                       `json:"review_sha256"`
	ResolvedSHA256           string                       `json:"resolved_sha256"`
	Renderer                 redaction.Recipe             `json:"renderer"`
	Namespace                NumberedNamespace            `json:"namespace"`
	PageCount                int                          `json:"page_count"`
	Pages                    []redaction.Page             `json:"pages"`
	Footer                   pdfproduction.NumberedFooter `json:"footer"`
	OriginalRetention        string                       `json:"original_retention"`
	OriginalReleaseAvailable bool                         `json:"original_release_available"`
}

type NumberedReservationRequest struct {
	Preparation    RedactionPreparation `json:"preparation"`
	Masks          []RedactionMask      `json:"masks"`
	ReviewSHA256   string               `json:"review_sha256" pattern:"^[0-9a-f]{64}$"`
	NamespaceID    uuid.UUID            `json:"namespace_id" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
	StartAt        int64                `json:"start_at" minimum:"1"`
	RetainOriginal bool                 `json:"retain_original"`
}

type NumberedReservation struct {
	Contract      string                    `json:"contract"`
	Recipe        NumberedRecipe            `json:"recipe"`
	RecipeSHA256  string                    `json:"recipe_sha256"`
	AllocationID  uuid.UUID                 `json:"allocation_id"`
	SnapshotID    uuid.UUID                 `json:"snapshot_id"`
	RequestSHA256 string                    `json:"request_sha256"`
	State         string                    `json:"state"`
	Pages         []store.BatesArtifactPage `json:"pages"`
}

type NumberedDelivery struct {
	Reservation            NumberedReservation `json:"reservation"`
	ArtifactManifestSHA256 string              `json:"artifact_manifest_sha256"`
	LayoutSHA256           []string            `json:"layout_sha256"`
	EndorsementsSHA256     []string            `json:"endorsements_sha256"`
}

func BuildNumberedRecipe(vault uuid.UUID, namespace store.BatesNamespace, start int64,
	review RedactionReview,
) (NumberedRecipe, error) {
	id, err := uuid.Parse(namespace.NamespaceID)
	if err != nil {
		return NumberedRecipe{}, ErrRedactionInvalidRequest
	}
	r := NumberedRecipe{Contract: NumberedPDFContract, VaultUID: vault, Selection: review.Selection,
		PreparationSHA256: review.PreparationSHA256, DecisionsSHA256: review.DecisionsSHA256,
		ReviewSHA256: review.SHA256, ResolvedSHA256: review.Resolved.SHA256, Renderer: review.Recipe,
		Namespace: NumberedNamespace{id, namespace.Prefix, namespace.Suffix, namespace.Padding, start},
		PageCount: review.Resolved.PageCount, Pages: review.Resolved.Pages,
		Footer: pdfproduction.QualifiedNumberedFooter(review.Recipe), OriginalRetention: "indefinite"}
	return r, r.Qualify()
}

func (r NumberedRecipe) SHA256() (string, error) {
	data, err := canonical.Marshal(r)
	if err != nil {
		return "", err
	}
	if len(data) > MaxNumberedRecipeBytes {
		return "", ErrRedactionPreparationLimit
	}
	return sha256HexBytes(data), nil
}

func DecodeNumberedRecipe(data []byte) (NumberedRecipe, error) {
	var r NumberedRecipe
	if len(data) > MaxNumberedRecipeBytes {
		return r, ErrRedactionPreparationLimit
	}
	if err := json.Unmarshal(data, &r, json.RejectUnknownMembers(true)); err != nil {
		return r, err
	}
	return r, ValidateNumberedRecipe(r)
}

func ValidateNumberedRecipe(r NumberedRecipe) error {
	if r.Contract != NumberedPDFContract || canonical.ValidateUUIDv4(r.VaultUID.String()) != nil ||
		canonical.ValidateUUIDv4(r.Namespace.ID.String()) != nil || canonical.ValidateUUIDv4(r.Selection.Source.VersionID) != nil ||
		r.Selection.NodeID <= 0 || r.Selection.Source.Size < 1 || r.Selection.Source.Size > 64<<20 ||
		!canonical.IsSHA256Hex(r.Selection.Source.SHA256) || r.PageCount < 1 || r.PageCount > 16 || len(r.Pages) != r.PageCount ||
		r.OriginalRetention != "indefinite" || r.OriginalReleaseAvailable {
		return ErrRedactionInvalidRequest
	}
	for _, digest := range []string{r.PreparationSHA256, r.DecisionsSHA256, r.ReviewSHA256, r.ResolvedSHA256} {
		if !canonical.IsSHA256Hex(digest) {
			return ErrRedactionInvalidRequest
		}
	}
	n := r.Namespace
	if n.Start < 1 || n.Padding < 1 || n.Padding > 10 || !utf8.ValidString(n.Prefix) || !utf8.ValidString(n.Suffix) ||
		utf8.RuneCountInString(n.Prefix) > 128 || utf8.RuneCountInString(n.Suffix) > 128 {
		return ErrRedactionInvalidRequest
	}
	maxNumber := int64(1)
	for range n.Padding {
		maxNumber *= 10
	}
	if n.Start > maxNumber-int64(r.PageCount) {
		return store.ErrBatesOverflow
	}
	f := r.Footer
	if f.StripHeight <= 0 || f.StripHeight > document.MaxPageInteger || f.InsetX < 0 || f.InsetX > document.MaxPageInteger/2 ||
		f.InsetTop < 0 || f.InsetTop >= f.StripHeight || f.InsetBottom < 0 || f.InsetBottom >= f.StripHeight-f.InsetTop ||
		f.FontSHA256 != r.Renderer.FontSHA256 || f.FontSizeMilliPoints <= 0 {
		return ErrRedactionInvalidRequest
	}
	end := int64(0)
	for i, page := range r.Pages {
		if page.Width <= 2*f.InsetX || page.Height <= 0 || page.Width > document.MaxPageInteger || page.Height > document.MaxPageInteger-f.StripHeight ||
			page.Number != i+1 || !canonical.IsSHA256Hex(page.FrameSHA256) || page.Span.Start != end || page.Span.End < end {
			return ErrRedactionInvalidRequest
		}
		end = page.Span.End
	}
	rr := r.Renderer
	if rr.Contract != "raster-redaction/v1" || !canonical.IsSHA256Hex(rr.RendererSHA256) ||
		!canonical.IsSHA256Hex(rr.FontSHA256) || rr.WriterVersion == "" || rr.DPI <= 0 || rr.PaddingPixels < 0 ||
		rr.MaxPixels <= 0 || rr.MaxAxis <= 0 || rr.WASMMemoryBytes <= 0 || rr.PageTimeoutSeconds <= 0 ||
		rr.QualifiedPeakRSSBytes <= 0 || rr.MaxStagingBytes <= 0 {
		return ErrRedactionInvalidRequest
	}
	return nil
}

// Qualify is required only when executing or fully re-verifying the recipe.
func (r NumberedRecipe) Qualify() error {
	if err := ValidateNumberedRecipe(r); err != nil {
		return err
	}
	if r.Renderer != pdfproduction.QualifiedRecipe() || r.Footer != pdfproduction.QualifiedNumberedFooter(r.Renderer) {
		return pdfproduction.ErrVerifierVersion
	}
	presentation, err := r.Presentation()
	if err != nil {
		return err
	}
	return pdfproduction.PreflightPresentation(presentation, r.Renderer)
}

func (r NumberedRecipe) Labels() []string {
	labels := make([]string, r.PageCount)
	for i := range labels {
		labels[i] = fmt.Sprintf("%s%0*d%s", r.Namespace.Prefix, r.Namespace.Padding, r.Namespace.Start+int64(i), r.Namespace.Suffix)
	}
	return labels
}

func (r NumberedRecipe) Presentation() ([]pdfproduction.PagePresentation, error) {
	return pdfproduction.NumberedPresentation(r.Pages, r.Labels(), r.Footer)
}

func (r NumberedRecipe) MatchReview(review RedactionReview) error {
	if err := r.Qualify(); err != nil {
		return err
	}
	expected, err := BuildNumberedRecipe(r.VaultUID, store.BatesNamespace{NamespaceID: r.Namespace.ID.String(),
		Prefix: r.Namespace.Prefix, Suffix: r.Namespace.Suffix, Padding: r.Namespace.Padding}, r.Namespace.Start, review)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, r) {
		return ErrRedactionReviewChanged
	}
	return nil
}

func (r NumberedRecipe) occurrence() (string, error) {
	data, err := canonical.Marshal(struct {
		Vault   uuid.UUID `json:"vault_uid"`
		Node    int64     `json:"node_id"`
		Version string    `json:"version_id"`
	}{r.VaultUID, r.Selection.NodeID, r.Selection.Source.VersionID})
	if err != nil {
		return "", err
	}
	return sha256HexBytes(data)[:32], nil
}

func (r NumberedRecipe) PlanRequest(snapshot uuid.UUID) (store.BatesPlanRequest, error) {
	digest, err := r.SHA256()
	if err != nil {
		return store.BatesPlanRequest{}, err
	}
	occurrence, err := r.occurrence()
	if err != nil {
		return store.BatesPlanRequest{}, err
	}
	result := store.BatesPlanRequest{NamespaceID: r.Namespace.ID.String(), SnapshotID: snapshot.String(), RecipeSHA256: digest, StartAt: r.Namespace.Start}
	for i := range r.PageCount {
		result.Pages = append(result.Pages, store.BatesPageInput{
			OccurrenceID: occurrence, UnstampedSHA256: r.Selection.Source.SHA256, SourcePage: i + 1, VerifiedPageCount: r.PageCount})
	}
	return result, nil
}

func ValidateNumberedReservation(r NumberedReservation, review *RedactionReview) error {
	if r.Contract != "native-pdf-numbered-reservation/v1" || canonical.ValidateUUIDv4(r.AllocationID.String()) != nil ||
		canonical.ValidateUUIDv4(r.SnapshotID.String()) != nil || r.State != "reserved" && r.State != "committed" {
		return ErrRedactionInvalidRequest
	}
	if err := ValidateNumberedRecipe(r.Recipe); err != nil {
		return err
	}
	if review != nil {
		if err := r.Recipe.MatchReview(*review); err != nil {
			return err
		}
	}
	request, err := r.Recipe.PlanRequest(r.SnapshotID)
	if err != nil {
		return err
	}
	digest, err := store.BatesRequestDigest(request)
	if err != nil {
		return err
	}
	if r.RecipeSHA256 != request.RecipeSHA256 || digest != r.RequestSHA256 || len(r.Pages) != r.Recipe.PageCount {
		return ErrRedactionPreparationIntegrity
	}
	occurrence, err := r.Recipe.occurrence()
	if err != nil {
		return err
	}
	for i, label := range r.Recipe.Labels() {
		p := store.BatesArtifactPage{Ordinal: i + 1, OccurrenceID: occurrence, SourceBlobSHA256: r.Recipe.Selection.Source.SHA256, SourcePage: i + 1, OutputPage: i + 1, Label: label}
		if r.Pages[i] != p {
			return ErrRedactionPreparationIntegrity
		}
	}
	return nil
}

// CheckBatesArtifact binds retained bytes and labels to their checked recipe.
// A non-nil reservation selects numbered delivery; nil selects ordinary stamping.
func CheckBatesArtifact(artifact store.BatesArtifact, a store.BatesAllocation, ns store.BatesNamespace) (*NumberedReservation, error) {
	if artifact.State != "verified" || artifact.MediaType != "application/pdf" || artifact.Size < 1 || artifact.Size > 512<<20 ||
		artifact.ArtifactID != artifact.AllocationID || a.AllocationID != artifact.AllocationID || a.State != "committed" ||
		a.NamespaceID != ns.NamespaceID || a.RecipeSHA256 != artifact.RecipeSHA256 || len(a.Labels) != artifact.PageCount || len(artifact.Pages) != artifact.PageCount ||
		artifact.PageCount < 1 || a.EndSequence-a.StartSequence+1 != int64(artifact.PageCount) || len(artifact.RecipeJSON) > MaxNumberedRecipeBytes {
		return nil, ErrRedactionPreparationIntegrity
	}
	var contract struct {
		Contract string `json:"contract"`
	}
	if err := json.Unmarshal(artifact.RecipeJSON, &contract); err != nil {
		return nil, err
	}
	manifest, err := store.BatesArtifactManifestSHA(artifact)
	if err != nil || manifest != artifact.ManifestSHA256 {
		return nil, ErrRedactionPreparationIntegrity
	}
	for i, p := range artifact.Pages {
		label := a.Labels[i]
		if p.Ordinal != i+1 || p.OutputPage != i+1 || label.Ordinal != p.Ordinal || label.OutputPage != p.OutputPage ||
			label.SourcePage != p.SourcePage || label.OccurrenceID != p.OccurrenceID || label.Label != p.Label ||
			!canonical.IsSHA256Hex(p.SourceBlobSHA256) {
			return nil, ErrRedactionPreparationIntegrity
		}
	}
	switch contract.Contract {
	case NumberedPDFContract:
		recipe, err := DecodeNumberedRecipe(artifact.RecipeJSON)
		if err != nil {
			return nil, err
		}
		if recipe.Namespace.ID.String() != ns.NamespaceID || recipe.Namespace.Prefix != ns.Prefix || recipe.Namespace.Suffix != ns.Suffix ||
			recipe.Namespace.Padding != ns.Padding || recipe.Namespace.Start != a.StartSequence {
			return nil, ErrRedactionPreparationIntegrity
		}
		allocationID, err := uuid.Parse(a.AllocationID)
		if err != nil {
			return nil, fmt.Errorf("allocation identity: %w", err)
		}
		snapshotID, err := uuid.Parse(a.SnapshotID)
		if err != nil {
			return nil, fmt.Errorf("snapshot identity: %w", err)
		}
		r := NumberedReservation{Contract: "native-pdf-numbered-reservation/v1", Recipe: recipe, RecipeSHA256: a.RecipeSHA256,
			AllocationID: allocationID, SnapshotID: snapshotID, RequestSHA256: a.RequestSHA256, State: a.State, Pages: artifact.Pages}
		if err := ValidateNumberedReservation(r, nil); err != nil {
			return nil, err
		}
		if _, err := NumberedArtifactDelivery(r, artifact); err != nil {
			return nil, err
		}
		return &r, nil
	case pdfstamp.RecipeContractV1:
		var recipe pdfstamp.Recipe
		if err := json.Unmarshal(artifact.RecipeJSON, &recipe, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		if err := recipe.ValidateRecorded(); err != nil {
			return nil, err
		}
		encoded, err := canonical.Marshal(recipe)
		if err != nil || !bytes.Equal(encoded, artifact.RecipeJSON) || sha256HexBytes(encoded) != artifact.RecipeSHA256 || recipe.NamespaceID != ns.NamespaceID ||
			recipe.Prefix != ns.Prefix || recipe.Suffix != ns.Suffix || recipe.Padding != ns.Padding || int64(recipe.StartAt) != a.StartSequence {
			return nil, ErrRedactionPreparationIntegrity
		}
		for i, p := range artifact.Pages {
			if p.Label != fmt.Sprintf("%s%0*d%s", ns.Prefix, ns.Padding, a.StartSequence+int64(i), ns.Suffix) {
				return nil, ErrRedactionPreparationIntegrity
			}
		}
		return nil, nil //nolint:nilnil // A checked ordinary stamp has no numbered reservation.
	default:
		return nil, ErrRedactionPreparationIntegrity
	}
}

// RedactionReceiptPresentation binds an offline numbered receipt to the supplied
// review. It authenticates neither issuance nor the unsigned vault assertion.
func RedactionReceiptPresentation(receipt RedactionReceipt, review RedactionReview) ([]pdfproduction.PagePresentation, error) {
	if receipt.Contract == "native-pdf-redaction-result/v1" {
		if receipt.Numbered != nil || receipt.AdmittedNodeRevision < 1 {
			return nil, ErrRedactionPreparationIntegrity
		}
		return nil, nil
	}
	if receipt.Contract != "native-pdf-numbered-result/v1" || receipt.Numbered == nil || receipt.AdmittedNodeRevision != 0 {
		return nil, ErrRedactionPreparationIntegrity
	}
	n := receipt.Numbered
	r := n.Reservation
	if r.State != "committed" {
		return nil, ErrRedactionPreparationIntegrity
	}
	if err := ValidateNumberedReservation(r, &review); err != nil {
		return nil, err
	}
	encoded, err := canonical.Marshal(r.Recipe)
	if err != nil {
		return nil, err
	}
	artifact := store.BatesArtifact{ArtifactID: r.AllocationID.String(), AllocationID: r.AllocationID.String(),
		BlobSHA256: receipt.PDF.SHA256, Size: receipt.PDF.Size, MediaType: "application/pdf", PageCount: r.Recipe.PageCount,
		RecipeJSON: encoded, RecipeSHA256: r.RecipeSHA256, ManifestSHA256: n.ArtifactManifestSHA256, Pages: r.Pages}
	expected, err := NumberedArtifactDelivery(r, artifact)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(expected, n) {
		return nil, ErrRedactionPreparationIntegrity
	}
	return r.Recipe.Presentation()
}
