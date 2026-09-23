package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/png"
	"io"
	"math"
	"os"
	"slices"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

// ProductionPDFSourceOpener must use the stored finalized member and catalog
// relation to authorize its verified source stream. A hash alone is not access
// authority. The caller cannot supply PDF bytes through FinalizedProduction.
type ProductionPDFSourceOpener interface {
	OpenPinnedProductionPDF(ctx context.Context, job Job, prepared documentproduction.PreparedMember) (PinnedProductionPDF, error)
}

// ProductionPageCandidate is one final endorsed PNG in a private file. The
// stager must consume and verify exactly Size bytes, then atomically retain
// the artifact and its private stage receipt under the fenced claim.
type ProductionPageCandidate struct {
	File   *os.File
	SHA256 string
	Size   int64
}

type ProductionPageStager interface {
	StageProductionPage(ctx context.Context, claim JobClaim, job Job, plan RenderPlan,
		prepared documentproduction.PreparedMember, page RenderPagePlan, candidate ProductionPageCandidate) (ProductionPageStage, error)
}

type boundedPageWriter struct {
	writer io.Writer
	limit  int64
	wrote  int64
}

func (w *boundedPageWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.wrote {
		return 0, ErrJobConflict
	}
	n, err := w.writer.Write(p)
	w.wrote += int64(n)
	return n, err
}

// RenderProductionPages consumes one fully verified source at a time and
// stages one burned and endorsed final page at a time. StageProductionPage
// owns claim fencing and durable receipt replay; this function never
// publishes a job or registers a callable runtime route.
func RenderProductionPages(ctx context.Context, opener ProductionPDFSourceOpener, stager ProductionPageStager,
	engine pdfproduction.Engine, claim JobClaim, job Job, finalized FinalizedProduction, plan RenderPlan,
	recipe redaction.Recipe) error {
	return renderProductionPagesWithTimeout(ctx, opener, stager, engine, claim, job, finalized, plan,
		recipe, productionPageTimeout)
}

func renderProductionPagesWithTimeout(ctx context.Context, opener ProductionPDFSourceOpener, stager ProductionPageStager,
	engine pdfproduction.Engine, claim JobClaim, job Job, finalized FinalizedProduction, plan RenderPlan,
	recipe redaction.Recipe, pageTimeout time.Duration) error {
	if ctx == nil || opener == nil || stager == nil || engine == nil || claim.JobID != job.ID ||
		pageTimeout <= 0 || pageTimeout > productionPageTimeout ||
		job.ID != plan.JobID || job.RevisionSHA256 != plan.RevisionSHA256 ||
		finalized.Draft.SetID != job.SetID || finalized.Draft.Revision != job.Revision ||
		finalized.Authority.Prepared.SHA256 != job.RevisionSHA256 || finalized.Authority.Receipt == nil ||
		finalized.Authority.Receipt.SHA256 != job.PreparedInputSHA256 || len(finalized.Authority.Prepared.Members) == 0 {
		return ErrJobConflict
	}
	canonicalPlan, _, err := CanonicalRenderPlan(plan)
	if err != nil || canonicalPlan.SHA256 != plan.SHA256 {
		return ErrJobConflict
	}
	prepared := finalized.Authority.Prepared
	qualified, err := pdfproduction.QualifiedRecipeForDPI(recipe.DPI)
	recipeRaw, marshalErr := canonical.Marshal(recipe)
	if err != nil || marshalErr != nil || recipe != qualified ||
		hashProductionStageBytes(recipeRaw) != prepared.RecipeSHA256 {
		return ErrJobConflict
	}
	pageIndex := 0
	for _, member := range prepared.Members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if member.ResolvedSHA256 != member.Resolved.SHA256 || member.Resolved.RecipeSHA256 != prepared.RecipeSHA256 ||
			member.Member.PDFSize < 1 || member.Member.PDFSize > math.MaxUint32 {
			return ErrJobConflict
		}
		endorsed, err := PlanEndorsementPages(member.Member.ID, member.Resolved.Pages, member.Resolved,
			plan.Reservation.Numbers, recipe)
		if err != nil || len(endorsed) != len(member.Resolved.Pages) || pageIndex+len(endorsed) > len(plan.Pages) {
			return ErrJobConflict
		}
		for index, page := range endorsed {
			planned := plan.Pages[pageIndex+index]
			if planned.MemberID != member.Member.ID || planned.MemberOrdinal != member.Member.Ordinal ||
				planned.Page != member.Resolved.Pages[index].Number || planned.ResolvedSHA256 != member.ResolvedSHA256 ||
				planned.Layout != page.Layout || !slices.Equal(planned.Endorsements, page.Endorsements) {
				return ErrJobConflict
			}
		}
		pageIndex += len(endorsed)
	}
	if pageIndex != len(plan.Pages) {
		return ErrJobConflict
	}
	pageIndex = 0
	for _, member := range prepared.Members {
		if err := ctx.Err(); err != nil {
			return err
		}
		pinned, err := opener.OpenPinnedProductionPDF(ctx, job, member)
		if err != nil {
			return err
		}
		if pinned.PDFSHA256 != member.Member.PDFSHA256 || pinned.Size != member.Member.PDFSize {
			if pinned.Stream != nil {
				_ = pinned.Stream.Close()
			}
			return ErrJobConflict
		}
		spool, err := SpoolProductionPDF(ctx, pinned, min(recipe.MaxStagingBytes, int64(math.MaxUint32)))
		if err != nil {
			return err
		}
		for index := range member.Resolved.Pages {
			page := plan.Pages[pageIndex+index]
			pageCtx, stopPage := context.WithTimeout(ctx, pageTimeout)
			err := renderOneProductionPage(pageCtx, stager, engine, claim, job, plan, member, page,
				spool.File, pinned, recipe)
			err = errors.Join(err, pageCtx.Err())
			stopPage()
			if err != nil {
				return errors.Join(err, spool.Close())
			}
		}
		if err := spool.Close(); err != nil {
			return err
		}
		pageIndex += len(member.Resolved.Pages)
	}
	return ctx.Err()
}

func renderOneProductionPage(ctx context.Context, stager ProductionPageStager, engine pdfproduction.Engine,
	claim JobClaim, job Job, plan RenderPlan, member documentproduction.PreparedMember, page RenderPagePlan,
	source *os.File, pinned PinnedProductionPDF, recipe redaction.Recipe) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	raster, err := engine.Render(ctx, pdfproduction.Source{Reader: source, Size: pinned.Size, SHA256: pinned.PDFSHA256},
		page.Layout.Source, recipe)
	if err != nil {
		return err
	}
	var masks []redaction.Box
	for _, box := range member.Resolved.RedactBoxes {
		if box.Page == page.Page {
			masks = append(masks, box)
		}
	}
	burned, err := pdfproduction.Burn(raster, masks, recipe)
	if err != nil {
		return err
	}
	final, err := pdfproduction.Endorse(burned, page.Layout, page.Endorsements, recipe)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "docbank-production-page-*.png")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close(), os.Remove(file.Name())) }()
	hasher := sha256.New()
	bounded := &boundedPageWriter{writer: io.MultiWriter(file, hasher), limit: recipe.MaxStagingBytes}
	if err := png.Encode(bounded, final.Pixels); err != nil || bounded.wrote < 1 {
		return errors.Join(ErrJobConflict, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	candidate := ProductionPageCandidate{File: file, SHA256: hex.EncodeToString(hasher.Sum(nil)), Size: bounded.wrote}
	stage, err := stager.StageProductionPage(ctx, claim, job, plan, member, page, candidate)
	if err != nil {
		return err
	}
	if stage.Artifact.SHA256 != candidate.SHA256 || stage.Artifact.Size != candidate.Size {
		return ErrJobConflict
	}
	want, err := BuildProductionPageStage(job, plan, member, page, stage.Artifact)
	if err != nil || stage != want {
		return ErrJobConflict
	}
	return ctx.Err()
}
