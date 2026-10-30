package main

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

type recipientSelection struct {
	Contract  string           `json:"contract"`
	Documents []recipientInput `json:"documents"`
}
type recipientInput struct {
	AllocationID string `json:"allocation_id"`
	Preparation  string `json:"preparation"`
	Masks        string `json:"masks"`
	Review       string `json:"review"`
}
type recipientPreflight struct {
	Inputs    []recipientInput
	Receipts  []api.BatesExport
	Documents []processing.RecipientPackageDocument
}

func readRecipientSelection(path string) (recipientSelection, error) {
	var selection recipientSelection
	data, err := readRedactionFile(path, 256<<10)
	if err != nil {
		return selection, err
	}
	if err = json.Unmarshal(data, &selection, json.RejectUnknownMembers(true)); err != nil {
		return selection, usageError(err)
	}
	if selection.Contract != "native-pdf-recipient-selection/v1" || len(selection.Documents) < 1 || len(selection.Documents) > processing.MaxRecipientPackageDocuments {
		return selection, usageError(errors.New("selection requires native-pdf-recipient-selection/v1 and 1–16 documents"))
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return selection, err
	}
	seen := map[string]bool{}
	for i := range selection.Documents {
		d := &selection.Documents[i]
		if !daemonconn.IsCanonicalUUIDv4(d.AllocationID) || seen[d.AllocationID] {
			return selection, usageError(errors.New("allocations must be distinct canonical lowercase UUIDv4 values"))
		}
		seen[d.AllocationID] = true
		for _, p := range []*string{&d.Preparation, &d.Masks, &d.Review} {
			if *p == "" || len(*p) > 4096 || !utf8.ValidString(*p) || strings.ContainsRune(*p, 0) {
				return selection, usageError(errors.New("each preparation, masks and review path must contain 1–4096 UTF-8 bytes without NUL"))
			}
			if !filepath.IsAbs(*p) {
				*p = filepath.Join(base, *p)
			}
		}
	}
	return selection, nil
}

func preflightRecipientPackage(ctx context.Context, c *daemonconn.Connection, selection recipientSelection) (recipientPreflight, error) {
	checked := recipientPreflight{Inputs: selection.Documents}
	var numbered []*processing.NumberedReservation
	for i, input := range selection.Documents {
		receipt, err := c.BatesExport(ctx, input.AllocationID)
		if err != nil {
			return checked, err
		}
		n, err := daemonconn.CheckNumberedBatesExport(receipt)
		if err != nil {
			return checked, err
		}
		if n == nil {
			return checked, usageError(fmt.Errorf("allocation %s is a stamped export; select completed numbered redactions", input.AllocationID))
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
		return checked, recipientCLIError(err)
	}
	// Do all cheap local admission before any content request. Only digests survive.
	for i, input := range selection.Documents {
		if err := ctx.Err(); err != nil {
			return checked, err
		}
		review, err := readRecipientReview(input)
		if err != nil {
			return checked, fmt.Errorf("allocation %s: %w", input.AllocationID, err)
		}
		if err = numbered[i].Recipe.MatchReview(review); err != nil {
			return checked, redactionCLIError(err)
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

func readRecipientReview(input recipientInput) (processing.RedactionReview, error) {
	request, err := readRedactionInputs(input.Preparation, input.Masks)
	if err != nil {
		return processing.RedactionReview{}, err
	}
	review, err := readRedactionReview(input.Review, request)
	if err != nil {
		return review, err
	}
	size := int64(review.Resolved.PageCount)
	for _, run := range review.Resolved.Runs {
		size += int64(len(run.Text))
		if size > processing.MaxRecipientPackageTextBytes {
			return review, usageError(errors.New("reviewed text exceeds 16 MiB"))
		}
	}
	return review, nil
}

func recipientCLIError(err error) error {
	if errors.Is(err, processing.ErrRecipientPackageInvalid) || errors.Is(err, processing.ErrRecipientPackageLimit) {
		return usageError(err)
	}
	if errors.Is(err, processing.ErrRecipientPackageIntegrity) {
		return integrityError(err)
	}
	return redactionCLIError(err)
}
