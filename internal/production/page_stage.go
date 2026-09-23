package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"slices"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/kit/packstore"
)

const ProductionPageStageContractV1 = "production-page-stage/v1"
const MaxProductionPageStageBytes = 16 << 10

// ProductionPageStage is private authority retained with one final PNG.
// It binds the physical image to its occurrence, sealed plan and reservation.
type ProductionPageStage struct {
	Contract            string                      `json:"contract"`
	JobID               string                      `json:"job_id"`
	PreparedInputSHA256 string                      `json:"prepared_input_sha256"`
	RevisionSHA256      string                      `json:"revision_sha256"`
	RenderPlanSHA256    string                      `json:"render_plan_sha256"`
	ReservationSHA256   string                      `json:"reservation_sha256"`
	MemberID            string                      `json:"member_id"`
	MemberOrdinal       int64                       `json:"member_ordinal"`
	Page                int                         `json:"page"`
	ResolvedSHA256      string                      `json:"resolved_sha256"`
	RecipeSHA256        string                      `json:"recipe_sha256"`
	LayoutSHA256        string                      `json:"layout_sha256"`
	EndorsementsSHA256  string                      `json:"endorsements_sha256"`
	Artifact            documentproduction.Artifact `json:"artifact"`
	SHA256              string                      `json:"sha256"`
}

func CanonicalProductionPageStage(value ProductionPageStage) (ProductionPageStage, []byte, error) {
	if value.Contract != ProductionPageStageContractV1 || !canonicalUUIDv4(value.JobID) || !canonicalUUIDv4(value.MemberID) ||
		value.MemberOrdinal < 1 || value.Page < 1 || value.Artifact.Role != documentproduction.ArtifactRoleRedactedPage ||
		value.Artifact.MemberID != value.MemberID || value.Artifact.MemberOrdinal != value.MemberOrdinal ||
		value.Artifact.Page != value.Page || value.Artifact.MediaType != "image/png" || value.Artifact.Size < 1 {
		return ProductionPageStage{}, nil, ErrJobConflict
	}
	for _, digest := range []string{value.RevisionSHA256, value.PreparedInputSHA256, value.RenderPlanSHA256, value.ReservationSHA256, value.ResolvedSHA256,
		value.RecipeSHA256, value.LayoutSHA256, value.EndorsementsSHA256, value.Artifact.SHA256} {
		if !canonical.IsSHA256Hex(digest) {
			return ProductionPageStage{}, nil, ErrJobConflict
		}
	}
	if _, _, err := documentproduction.CanonicalArtifactManifest(documentproduction.ArtifactManifest{
		Contract: documentproduction.ArtifactManifestContractV1, Artifacts: []documentproduction.Artifact{value.Artifact},
	}); err != nil {
		return ProductionPageStage{}, nil, ErrJobConflict
	}
	claimed := value.SHA256
	value.SHA256 = ""
	raw, err := canonical.Marshal(value)
	if err != nil || len(raw) > MaxProductionPageStageBytes {
		return ProductionPageStage{}, nil, ErrJobConflict
	}
	sum := sha256.Sum256(raw)
	value.SHA256 = hex.EncodeToString(sum[:])
	if claimed != "" && claimed != value.SHA256 {
		return ProductionPageStage{}, nil, ErrJobConflict
	}
	stored, err := canonical.Marshal(value)
	if err != nil || len(stored) > MaxProductionPageStageBytes {
		return ProductionPageStage{}, nil, ErrJobConflict
	}
	return value, stored, nil
}

func BuildProductionPageStage(job Job, plan RenderPlan, prepared documentproduction.PreparedMember,
	page RenderPagePlan, artifact documentproduction.Artifact) (ProductionPageStage, error) {
	canonicalPlan, _, err := CanonicalRenderPlan(plan)
	if err != nil || canonicalPlan.SHA256 != plan.SHA256 || job.ID != plan.JobID || job.RevisionSHA256 != plan.RevisionSHA256 ||
		prepared.Member.ID != page.MemberID || prepared.Member.Ordinal != page.MemberOrdinal ||
		prepared.ResolvedSHA256 != page.ResolvedSHA256 || !slices.ContainsFunc(prepared.Resolved.Pages, func(p redaction.Page) bool { return p.Number == page.Page && p == page.Layout.Source }) {
		return ProductionPageStage{}, ErrJobConflict
	}
	if !slices.ContainsFunc(plan.Pages, func(candidate RenderPagePlan) bool { return reflect.DeepEqual(candidate, page) }) {
		return ProductionPageStage{}, ErrJobConflict
	}
	layoutRaw, err := canonical.Marshal(page.Layout)
	if err != nil {
		return ProductionPageStage{}, err
	}
	endorsements := page.Endorsements
	if endorsements == nil {
		endorsements = []redaction.Endorsement{}
	}
	endorsementRaw, err := canonical.Marshal(endorsements)
	if err != nil {
		return ProductionPageStage{}, err
	}
	value := ProductionPageStage{Contract: ProductionPageStageContractV1, JobID: job.ID, RevisionSHA256: job.RevisionSHA256,
		PreparedInputSHA256: job.PreparedInputSHA256, RenderPlanSHA256: plan.SHA256,
		ReservationSHA256: plan.Reservation.SHA256, MemberID: page.MemberID,
		MemberOrdinal: page.MemberOrdinal, Page: page.Page, ResolvedSHA256: page.ResolvedSHA256,
		RecipeSHA256: prepared.Resolved.RecipeSHA256, LayoutSHA256: hashProductionStageBytes(layoutRaw),
		EndorsementsSHA256: hashProductionStageBytes(endorsementRaw), Artifact: artifact}
	value, _, err = CanonicalProductionPageStage(value)
	return value, err
}

func hashProductionStageBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type ProductionPageHandleStore interface {
	LoadProductionPageStage(ctx context.Context, jobID, memberID string, page int) (ProductionPageStage, error)
	OpenStagedProductionPage(ctx context.Context, stage ProductionPageStage) (packstore.VerifiedReadCloser, int64, error)
}

// VerifiedProductionPageSequence reopens one retained final PNG at a time.
// Every Next rechecks private stage authority and physical bytes, including
// after a process restart. The writer must close OpenPNG before advancing.
type VerifiedProductionPageSequence struct {
	ctx         context.Context
	store       ProductionPageHandleStore
	job         Job
	plan        RenderPlan
	prepared    documentproduction.PreparedMember
	pages       []RenderPagePlan
	maxPNGBytes int64
	index       int
	pending     *VerifiedProductionFile
	opened      bool
}

func NewVerifiedProductionPageSequence(ctx context.Context, store ProductionPageHandleStore, job Job,
	plan RenderPlan, prepared documentproduction.PreparedMember, maxPNGBytes int64) (*VerifiedProductionPageSequence, error) {
	if ctx == nil || store == nil || maxPNGBytes < 1 || job.ID != plan.JobID ||
		prepared.Member.ID == "" {
		return nil, ErrJobConflict
	}
	canonicalPlan, _, err := CanonicalRenderPlan(plan)
	if err != nil || canonicalPlan.SHA256 != plan.SHA256 {
		return nil, ErrJobConflict
	}
	pages := make([]RenderPagePlan, 0, len(prepared.Resolved.Pages))
	for _, page := range plan.Pages {
		if page.MemberID == prepared.Member.ID {
			pages = append(pages, page)
		}
	}
	if len(pages) != len(prepared.Resolved.Pages) {
		return nil, ErrJobConflict
	}
	for index, page := range pages {
		if page.MemberOrdinal != prepared.Member.Ordinal || page.Page != index+1 ||
			page.ResolvedSHA256 != prepared.ResolvedSHA256 || page.Layout.Source != prepared.Resolved.Pages[index] {
			return nil, ErrJobConflict
		}
	}
	return &VerifiedProductionPageSequence{ctx: ctx, store: store, job: job, plan: plan, prepared: prepared, pages: pages, maxPNGBytes: maxPNGBytes}, nil
}

func (s *VerifiedProductionPageSequence) Next(ctx context.Context) (pdfproduction.PageArtifact, error) {
	if err := ctx.Err(); err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	if err := s.ctx.Err(); err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	if s.pending != nil {
		return pdfproduction.PageArtifact{}, ErrJobConflict
	}
	if s.index == len(s.pages) {
		return pdfproduction.PageArtifact{}, io.EOF
	}
	page := s.pages[s.index]
	stage, err := s.store.LoadProductionPageStage(ctx, s.job.ID, page.MemberID, page.Page)
	if err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	want, err := BuildProductionPageStage(s.job, s.plan, s.prepared, page, stage.Artifact)
	if err != nil || stage != want {
		return pdfproduction.PageArtifact{}, ErrJobConflict
	}
	stream, size, err := s.store.OpenStagedProductionPage(ctx, stage)
	if err != nil || stream == nil {
		if stream != nil {
			_ = stream.Close()
		}
		return pdfproduction.PageArtifact{}, errors.Join(ErrJobConflict, err)
	}
	spool, err := SpoolProductionPDF(ctx, PinnedProductionPDF{PDFSHA256: stage.Artifact.SHA256, Size: size, Stream: stream}, s.maxPNGBytes)
	if err != nil || size != stage.Artifact.Size {
		if spool != nil {
			_ = spool.Close()
		}
		return pdfproduction.PageArtifact{}, errors.Join(ErrJobConflict, err)
	}
	s.pending = spool
	s.index++
	var runs []redaction.Run
	for _, run := range s.prepared.Resolved.Runs {
		if run.Page == page.Page {
			runs = append(runs, run)
		}
	}
	return pdfproduction.PageArtifact{Page: page.Layout.Output, PNGSHA256: stage.Artifact.SHA256, PNGSize: stage.Artifact.Size,
		ResolvedSHA256: page.ResolvedSHA256, Layout: page.Layout, LayoutSHA256: stage.LayoutSHA256,
		Endorsements: slices.Clone(page.Endorsements), EndorsementsSHA256: stage.EndorsementsSHA256, Runs: runs,
		OpenPNG: func() (io.ReadCloser, error) {
			if s.pending != spool || spool.File == nil || s.opened {
				return nil, ErrJobConflict
			}
			s.opened = true
			return &productionPageReader{file: spool, owner: s}, nil
		}}, nil
}

func (s *VerifiedProductionPageSequence) Close() error {
	if s.pending == nil {
		return nil
	}
	spool := s.pending
	s.pending = nil
	s.opened = false
	return spool.Close()
}

type productionPageReader struct {
	file   *VerifiedProductionFile
	owner  *VerifiedProductionPageSequence
	closed bool
}

func (r *productionPageReader) Read(p []byte) (int, error) {
	if r.closed || r.file.File == nil {
		return 0, io.ErrClosedPipe
	}
	return r.file.File.Read(p)
}
func (r *productionPageReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.owner.pending == r.file {
		r.owner.pending = nil
		r.owner.opened = false
	}
	return r.file.Close()
}

var _ pdfproduction.PageSequence = (*VerifiedProductionPageSequence)(nil)
