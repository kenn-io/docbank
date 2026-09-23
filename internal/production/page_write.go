package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"strings"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

// VerifiedProductionPDF is a fresh private PDF that passed independent pixel
// and text verification against newly reopened staged page handles.
type VerifiedProductionPDF struct {
	*VerifiedProductionFile

	SHA256 string
	Size   int64
}

// WriteAndVerifyProductionMember reopens every retained PNG for the writer
// and again for the independent verifier. The caller must stage the returned
// private PDF under the same fenced job claim before publication.
func WriteAndVerifyProductionMember(ctx context.Context, handles ProductionPageHandleStore, job Job,
	plan RenderPlan, prepared documentproduction.PreparedMember, recipe redaction.Recipe) (result *VerifiedProductionPDF, resultErr error) {
	if ctx == nil || handles == nil {
		return nil, ErrJobConflict
	}
	qualified, err := pdfproduction.QualifiedRecipeForDPI(recipe.DPI)
	recipeRaw, marshalErr := canonical.Marshal(recipe)
	if err != nil || marshalErr != nil || recipe != qualified || hashProductionStageBytes(recipeRaw) != prepared.Resolved.RecipeSHA256 {
		return nil, ErrJobConflict
	}
	sequence, err := NewVerifiedProductionPageSequence(ctx, handles, job, plan, prepared, recipe.MaxStagingBytes)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, sequence.Close())
		if resultErr != nil && result != nil {
			resultErr = errors.Join(resultErr, result.Close())
			result = nil
		}
	}()
	file, err := os.CreateTemp("", "docbank-production-fresh-*.pdf")
	if err != nil {
		return nil, err
	}
	result = &VerifiedProductionPDF{VerifiedProductionFile: &VerifiedProductionFile{File: file}}
	hasher := sha256.New()
	bounded := &boundedPageWriter{writer: io.MultiWriter(file, hasher), limit: min(recipe.MaxStagingBytes, int64(math.MaxUint32))}
	if err := pdfproduction.WriteFresh(ctx, bounded, sequence, prepared.Resolved, recipe); err != nil {
		return result, err
	}
	if bounded.wrote < 1 {
		return result, ErrJobConflict
	}
	result.Size, result.SHA256 = bounded.wrote, hex.EncodeToString(hasher.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	verificationSequence, err := NewVerifiedProductionPageSequence(ctx, handles, job, plan, prepared, recipe.MaxStagingBytes)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, verificationSequence.Close()) }()
	pages := make([]redaction.Page, len(verificationSequence.pages))
	resolvedDigests := make([]string, len(pages))
	layoutDigests := make([]string, len(pages))
	endorsementDigests := make([]string, len(pages))
	for index, page := range verificationSequence.pages {
		pages[index] = page.Layout.Output
		resolvedDigests[index] = page.ResolvedSHA256
		layoutRaw, err := canonical.Marshal(page.Layout)
		if err != nil {
			return result, err
		}
		layoutDigests[index] = hashProductionStageBytes(layoutRaw)
		endorsements := page.Endorsements
		if endorsements == nil {
			endorsements = []redaction.Endorsement{}
		}
		endorsementRaw, err := canonical.Marshal(endorsements)
		if err != nil {
			return result, err
		}
		endorsementDigests[index] = hashProductionStageBytes(endorsementRaw)
	}
	textIndex, endorsementIndex := 0, 0
	input := pdfproduction.VerificationInput{Pages: pages, ResolvedSHA256: resolvedDigests,
		LayoutSHA256: layoutDigests, EndorsementsSHA256: endorsementDigests,
		NextPageArtifact: verificationSequence.Next,
		NextText: func(ctx context.Context) (int, []byte, error) {
			if err := ctx.Err(); err != nil {
				return 0, nil, err
			}
			if textIndex == len(pages) {
				return 0, nil, io.EOF
			}
			textIndex++
			var text strings.Builder
			for _, run := range prepared.Resolved.Runs {
				if run.Page == textIndex {
					text.WriteString(run.Text)
				}
			}
			return textIndex, []byte(text.String()), nil
		},
		NextEndorsements: func(ctx context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
			if err := ctx.Err(); err != nil {
				return redaction.PageLayout{}, nil, err
			}
			if endorsementIndex == len(pages) {
				return redaction.PageLayout{}, nil, io.EOF
			}
			page := verificationSequence.pages[endorsementIndex]
			endorsementIndex++
			return page.Layout, page.Endorsements, nil
		},
	}
	if err := pdfproduction.VerifyFreshWithRecipe(ctx, file, result.Size, input, recipe); err != nil {
		return result, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	return result, nil
}
