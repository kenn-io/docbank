package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactionfiles"
)

type RecipientSelection struct {
	Contract  string                  `json:"contract"`
	Documents []RecipientPackageInput `json:"documents"`
}
type RecipientPackageInput struct {
	AllocationID string `json:"allocation_id"`
	Preparation  string `json:"preparation"`
	Masks        string `json:"masks"`
	Review       string `json:"review"`
}
type RecipientPackagePlan struct {
	Inputs    []RecipientPackageInput
	Receipts  []api.BatesExport
	Documents []processing.RecipientPackageDocument
}

func ReadRecipientSelection(ctx context.Context, path string) (selection RecipientSelection, err error) {
	defer func() {
		if err != nil {
			err = &RecipientPackageInputError{err}
		}
	}()
	data, err := redactionfiles.ReadFile(ctx, path, 256<<10)
	if err != nil {
		return selection, err
	}
	if err = json.Unmarshal(data, &selection, json.RejectUnknownMembers(true)); err != nil {
		return selection, fmt.Errorf("%w: %w", processing.ErrRecipientPackageInvalid, err)
	}
	if selection.Contract != "native-pdf-recipient-selection/v1" || len(selection.Documents) < 1 {
		return selection, invalid("selection requires native-pdf-recipient-selection/v1 and 1–16 documents")
	}
	if len(selection.Documents) > processing.MaxRecipientPackageDocuments {
		return selection, processing.ErrRecipientPackageLimit
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return selection, err
	}
	seen := map[string]bool{}
	for i := range selection.Documents {
		d := &selection.Documents[i]
		if !IsCanonicalUUIDv4(d.AllocationID) || seen[d.AllocationID] {
			return selection, invalid("allocations must be distinct canonical lowercase UUIDv4 values")
		}
		seen[d.AllocationID] = true
		for _, p := range []*string{&d.Preparation, &d.Masks, &d.Review} {
			if *p == "" || len(*p) > 4096 || !utf8.ValidString(*p) || strings.ContainsRune(*p, 0) {
				return selection, invalid("each preparation, masks and review path must contain 1–4096 UTF-8 bytes without NUL")
			}
			if !filepath.IsAbs(*p) {
				*p = filepath.Join(base, *p)
			}
		}
	}
	return selection, nil
}

func PreflightRecipientPackage(ctx context.Context, c *Connection, selection RecipientSelection) (RecipientPackagePlan, error) {
	checked := RecipientPackagePlan{Inputs: selection.Documents}
	var numbered []*processing.NumberedReservation
	for i, input := range selection.Documents {
		receipt, err := c.BatesExport(ctx, input.AllocationID)
		if err != nil {
			return checked, err
		}
		n, err := CheckNumberedBatesExport(receipt)
		if err != nil {
			return checked, err
		}
		if n == nil {
			return checked, fmt.Errorf("%w: allocation %s is a stamped export; select completed numbered redactions", processing.ErrRecipientPackageInvalid, input.AllocationID)
		}
		labels := make([]string, len(receipt.Pages))
		for j, p := range receipt.Pages {
			labels[j] = p.Label
		}
		d := processing.RecipientPackageDocument{
			Ordinal: i + 1, DocumentID: labels[0], AllocationID: input.AllocationID,
			NamespaceID: n.Recipe.Namespace.ID.String(), ArtifactManifestSHA256: receipt.ManifestSHA256,
			RecipeSHA256: receipt.RecipeSHA256, ReviewSHA256: n.Recipe.ReviewSHA256,
			PageCount: receipt.PageCount, Labels: labels,
			PDF: processing.RecipientPackageFile{Path: fmt.Sprintf("VOL001/PDF/%06d.pdf", i+1), Size: receipt.Size, SHA256: receipt.BlobSHA256},
		}
		checked.Documents = append(checked.Documents, d)
		checked.Receipts = append(checked.Receipts, receipt)
		numbered = append(numbered, n)
	}
	if err := processing.ValidateRecipientDocuments(checked.Documents); err != nil {
		return checked, err
	}
	// Do all cheap local admission before any content request. Only digests survive.
	for i, input := range selection.Documents {
		if err := ctx.Err(); err != nil {
			return checked, err
		}
		review, err := readRecipientReview(ctx, input)
		if err != nil {
			return checked, fmt.Errorf("allocation %s: %w", input.AllocationID, err)
		}
		if err = numbered[i].Recipe.MatchReview(review); err != nil {
			return checked, err
		}
		checked.Documents[i].ReviewSHA256 = review.SHA256
		text, err := redaction.Text(review.Resolved)
		if err != nil {
			return checked, err
		}
		checked.Documents[i].Text = processing.RecipientPackageFile{
			Path: fmt.Sprintf("VOL001/TEXT/%06d.txt", i+1), Size: int64(len(text)),
			SHA256: fmt.Sprintf("%x", sha256.Sum256(text)),
		}
	}
	return checked, nil
}

func readRecipientReview(ctx context.Context, input RecipientPackageInput) (review processing.RedactionReview, err error) {
	defer func() {
		if err != nil {
			err = &RecipientPackageInputError{err}
		}
	}()
	request, err := redactionfiles.ReadInputs(ctx, input.Preparation, input.Masks)
	if err != nil {
		return processing.RedactionReview{}, err
	}
	review, err = redactionfiles.ReadReview(ctx, input.Review, request)
	if err != nil {
		return review, err
	}
	size := int64(review.Resolved.PageCount)
	for _, run := range review.Resolved.Runs {
		size += int64(len(run.Text))
		if size > processing.MaxRecipientPackageTextBytes {
			return review, fmt.Errorf("%w: reviewed text exceeds 16 MiB", processing.ErrRecipientPackageLimit)
		}
	}
	return review, nil
}

// RecipientPackageInputError retains private-reader causes for CLI diagnostics. MCP must drop them.
type RecipientPackageInputError struct{ cause error }

func (e *RecipientPackageInputError) Error() string { return e.cause.Error() }
func (e *RecipientPackageInputError) Unwrap() error { return e.cause }

func invalid(message string) error {
	return fmt.Errorf("%w: %s", processing.ErrRecipientPackageInvalid, message)
}
