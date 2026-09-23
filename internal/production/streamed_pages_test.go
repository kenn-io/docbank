package production

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/kit/packstore"
)

type syntheticVerifiedPDF struct {
	*bytes.Reader

	verified bool
	closed   bool
}

type syntheticPageHandles struct {
	stage     ProductionPageStage
	data      []byte
	openCount int
	verified  bool
	stageErr  error
	openErr   error
}

type syntheticPageArchive struct{ pages map[int]*syntheticPageHandles }

func (a *syntheticPageArchive) StageProductionPage(ctx context.Context, claim JobClaim, job Job, plan RenderPlan,
	prepared documentproduction.PreparedMember, page RenderPagePlan, candidate ProductionPageCandidate) (ProductionPageStage, error) {
	handle := &syntheticPageHandles{}
	stage, err := handle.StageProductionPage(ctx, claim, job, plan, prepared, page, candidate)
	if err != nil {
		return ProductionPageStage{}, err
	}
	if page.Page == 2 {
		artifact := stage.Artifact
		artifact.ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		artifact.Path = "VOL001/SYN000002.png"
		stage, err = BuildProductionPageStage(job, plan, prepared, page, artifact)
		if err != nil {
			return ProductionPageStage{}, err
		}
		handle.stage = stage
	}
	if a.pages == nil {
		a.pages = make(map[int]*syntheticPageHandles)
	}
	a.pages[page.Page] = handle
	return stage, nil
}

func (a *syntheticPageArchive) LoadProductionPageStage(_ context.Context, _, _ string, page int) (ProductionPageStage, error) {
	if handle := a.pages[page]; handle != nil {
		return handle.stage, nil
	}
	return ProductionPageStage{}, ErrJobStageMissing
}

func (a *syntheticPageArchive) OpenStagedProductionPage(ctx context.Context, stage ProductionPageStage) (packstore.VerifiedReadCloser, int64, error) {
	if handle := a.pages[stage.Page]; handle != nil {
		return handle.OpenStagedProductionPage(ctx, stage)
	}
	return nil, 0, ErrJobConflict
}

func (h *syntheticPageHandles) StageProductionPage(_ context.Context, _ JobClaim, job Job, plan RenderPlan,
	prepared documentproduction.PreparedMember, page RenderPagePlan, candidate ProductionPageCandidate) (ProductionPageStage, error) {
	if h.stageErr != nil {
		return ProductionPageStage{}, h.stageErr
	}
	data, err := io.ReadAll(candidate.File)
	if err != nil {
		return ProductionPageStage{}, err
	}
	if int64(len(data)) != candidate.Size || testHash(string(data)) != candidate.SHA256 {
		return ProductionPageStage{}, ErrJobConflict
	}
	h.data = data
	h.verified = true
	artifact := documentproduction.Artifact{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", MemberID: page.MemberID,
		MemberOrdinal: page.MemberOrdinal, Page: page.Page, Role: documentproduction.ArtifactRoleRedactedPage,
		Path: "VOL001/SYN000001.png", SHA256: candidate.SHA256, Size: candidate.Size, MediaType: "image/png", Volume: "VOL001"}
	h.stage, err = BuildProductionPageStage(job, plan, prepared, page, artifact)
	return h.stage, err
}

type syntheticProductionSource struct {
	data  []byte
	opens int
}

func (s *syntheticProductionSource) OpenPinnedProductionPDF(_ context.Context, _ Job, member documentproduction.PreparedMember) (PinnedProductionPDF, error) {
	s.opens++
	return syntheticPinnedPDF(s.data, &syntheticVerifiedPDF{Reader: bytes.NewReader(s.data), verified: true}, member.Member.PDFSize), nil
}

func (h *syntheticPageHandles) LoadProductionPageStage(_ context.Context, _, _ string, _ int) (ProductionPageStage, error) {
	if h.stage.SHA256 == "" {
		return ProductionPageStage{}, ErrJobStageMissing
	}
	return h.stage, nil
}

func (h *syntheticPageHandles) OpenStagedProductionPage(_ context.Context, _ ProductionPageStage) (packstore.VerifiedReadCloser, int64, error) {
	if h.openErr != nil {
		return nil, 0, h.openErr
	}
	h.openCount++
	return &syntheticVerifiedPDF{Reader: bytes.NewReader(h.data), verified: h.verified}, int64(len(h.data)), nil
}

func syntheticPageStageFixture(t *testing.T) (Job, RenderPlan, documentproduction.PreparedMember, *syntheticPageHandles) {
	t.Helper()
	prepared := newStoredFixture(t).finalized.Authority.Prepared.Members[0]
	const jobID = "77000000-0000-4000-8000-000000000001"
	job := Job{ID: jobID, RevisionSHA256: testHash("sealed revision"), PreparedInputSHA256: testHash("prepared input")}
	reservation := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1, Authority: "bates-ledger/v1",
		ID: "77000000-0000-4000-8000-000000000002", OperationID: jobID, RevisionSHA256: job.RevisionSHA256, State: "reserved",
		Numbers: []documentproduction.AssignedNumber{{MemberID: prepared.Member.ID, MemberOrdinal: 1, Page: 1, Text: "PLAN000001"}}}
	_, digest, err := documentproduction.CanonicalNumberReservation(reservation)
	require.NoError(t, err)
	reservation.SHA256 = digest
	page := prepared.Resolved.Pages[0]
	plan := RenderPlan{Contract: RenderPlanContractV1, JobID: jobID, RevisionSHA256: job.RevisionSHA256, Reservation: reservation,
		Pages: []RenderPagePlan{{MemberID: prepared.Member.ID, MemberOrdinal: 1, Page: page.Number, ResolvedSHA256: prepared.ResolvedSHA256,
			Layout: redaction.PageLayout{Source: page, Output: page}, Endorsements: []redaction.Endorsement{}}}}
	plan, _, err = CanonicalRenderPlan(plan)
	require.NoError(t, err)
	data := []byte("synthetic final PNG")
	artifact := documentproduction.Artifact{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", MemberID: prepared.Member.ID,
		MemberOrdinal: 1, Page: 1, Role: documentproduction.ArtifactRoleRedactedPage, Path: "VOL001/SYN000001.png",
		SHA256: testHash(string(data)), Size: int64(len(data)), MediaType: "image/png", Volume: "VOL001"}
	stage, err := BuildProductionPageStage(job, plan, prepared, plan.Pages[0], artifact)
	require.NoError(t, err)
	return job, plan, prepared, &syntheticPageHandles{stage: stage, data: data, verified: true}
}

func TestVerifiedProductionPageSequenceReopensAndBindsStage(t *testing.T) {
	job, plan, prepared, handles := syntheticPageStageFixture(t)
	sequence, err := NewVerifiedProductionPageSequence(t.Context(), handles, job, plan, prepared, 1<<20)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sequence.Close()) })
	page, err := sequence.Next(t.Context())
	require.NoError(t, err)
	require.Equal(t, plan.Pages[0].Layout, page.Layout)
	require.Equal(t, handles.stage.LayoutSHA256, page.LayoutSHA256)
	_, err = sequence.Next(t.Context())
	require.ErrorIs(t, err, ErrJobConflict)
	reader, err := page.OpenPNG()
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, handles.data, got)
	require.NoError(t, reader.Close())
	_, err = sequence.Next(t.Context())
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 1, handles.openCount)
}

func TestVerifiedProductionPageSequenceRejectsChangedPlanOrUnverifiedStage(t *testing.T) {
	job, plan, prepared, handles := syntheticPageStageFixture(t)
	changed := plan
	changed.Pages = append([]RenderPagePlan(nil), plan.Pages...)
	changed.Pages[0].Endorsements = []redaction.Endorsement{{Text: "changed"}}
	changed, _, err := CanonicalRenderPlan(RenderPlan{Contract: changed.Contract, JobID: changed.JobID, RevisionSHA256: changed.RevisionSHA256,
		Reservation: changed.Reservation, Pages: changed.Pages})
	require.NoError(t, err)
	sequence, err := NewVerifiedProductionPageSequence(t.Context(), handles, job, changed, prepared, 1<<20)
	require.NoError(t, err)
	_, err = sequence.Next(t.Context())
	require.ErrorIs(t, err, ErrJobConflict)
	require.Zero(t, handles.openCount)

	handles.verified = false
	sequence, err = NewVerifiedProductionPageSequence(t.Context(), handles, job, plan, prepared, 1<<20)
	require.NoError(t, err)
	_, err = sequence.Next(t.Context())
	require.ErrorIs(t, err, ErrJobConflict)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sequence, err = NewVerifiedProductionPageSequence(ctx, handles, job, plan, prepared, 1<<20)
	require.NoError(t, err)
	_, err = sequence.Next(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	missing := errors.New("synthetic missing staged blob")
	handles.verified = true
	handles.openErr = missing
	sequence, err = NewVerifiedProductionPageSequence(t.Context(), handles, job, plan, prepared, 1<<20)
	require.NoError(t, err)
	_, err = sequence.Next(t.Context())
	require.ErrorIs(t, err, ErrJobConflict)
	require.ErrorIs(t, err, missing)
}

func TestProductionPageStagesKeepRepeatedSourceOccurrencesDistinct(t *testing.T) {
	job, plan, first, firstHandles := syntheticPageStageFixture(t)
	second := first
	second.Member.ID = "55555555-5555-4555-8555-555555555555"
	second.Member.Ordinal = 2
	plan.Reservation.Numbers = append(plan.Reservation.Numbers, documentproduction.AssignedNumber{
		MemberID: second.Member.ID, MemberOrdinal: 2, Page: 1, Text: "PLAN000002"})
	plan.Reservation.SHA256 = ""
	_, digest, err := documentproduction.CanonicalNumberReservation(plan.Reservation)
	require.NoError(t, err)
	plan.Reservation.SHA256 = digest
	secondPage := plan.Pages[0]
	secondPage.MemberID = second.Member.ID
	secondPage.MemberOrdinal = 2
	plan.Pages = append(plan.Pages, secondPage)
	plan, _, err = CanonicalRenderPlan(RenderPlan{Contract: RenderPlanContractV1, JobID: job.ID,
		RevisionSHA256: job.RevisionSHA256, Reservation: plan.Reservation, Pages: plan.Pages})
	require.NoError(t, err)
	firstStage, err := BuildProductionPageStage(job, plan, first, plan.Pages[0], firstHandles.stage.Artifact)
	require.NoError(t, err)
	secondArtifact := firstHandles.stage.Artifact
	secondArtifact.ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	secondArtifact.MemberID = second.Member.ID
	secondArtifact.MemberOrdinal = 2
	secondArtifact.Path = "VOL001/SYN000002.png"
	secondStage, err := BuildProductionPageStage(job, plan, second, plan.Pages[1], secondArtifact)
	require.NoError(t, err)
	require.NotEqual(t, firstStage.SHA256, secondStage.SHA256)
	firstHandles.stage = firstStage
	secondHandles := &syntheticPageHandles{stage: secondStage, data: append([]byte(nil), firstHandles.data...), verified: true}
	for _, test := range []struct {
		member  documentproduction.PreparedMember
		handles *syntheticPageHandles
	}{
		{first, firstHandles}, {second, secondHandles},
	} {
		sequence, err := NewVerifiedProductionPageSequence(t.Context(), test.handles, job, plan, test.member, 1<<20)
		require.NoError(t, err)
		artifact, err := sequence.Next(t.Context())
		require.NoError(t, err)
		reader, err := artifact.OpenPNG()
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		_, err = sequence.Next(t.Context())
		require.ErrorIs(t, err, io.EOF)
		require.NoError(t, sequence.Close())
	}
	secondHandles.stage = firstStage
	sequence, err := NewVerifiedProductionPageSequence(t.Context(), secondHandles, job, plan, second, 1<<20)
	require.NoError(t, err)
	_, err = sequence.Next(t.Context())
	require.ErrorIs(t, err, ErrJobConflict)
}

func TestRenderProductionPagesVerifiedSourceAndFencedStage(t *testing.T) {
	fixture := newStoredFixture(t)
	finalized := fixture.finalized
	pdf := redactiontest.PDF(t, []string{"synthetic source"}, "synthetic production")
	member := &finalized.Authority.Prepared.Members[0]
	member.Member.PDFSHA256 = testHash(string(pdf))
	member.Member.PDFSize = int64(len(pdf))
	recipe := pdfproduction.QualifiedRecipe()
	member.Resolved = endorsementResolved(t, recipe, []redaction.Page{{Number: 1, FrameSHA256: testHash("wide frame"),
		Width: 85_000, Height: 110_000, Span: redaction.Span{Start: 0, End: 1}}}, nil)
	member.ResolvedSHA256 = member.Resolved.SHA256
	job, plan, _, _ := syntheticPageStageFixture(t)
	job.SetID = finalized.Draft.SetID
	job.Revision = finalized.Draft.Revision
	job.RevisionSHA256 = finalized.Authority.Prepared.SHA256
	job.PreparedInputSHA256 = finalized.Authority.Receipt.SHA256
	plan.RevisionSHA256 = job.RevisionSHA256
	plan.Reservation.RevisionSHA256 = job.RevisionSHA256
	_, digest, err := documentproduction.CanonicalNumberReservation(plan.Reservation)
	require.NoError(t, err)
	plan.Reservation.SHA256 = digest
	plan.Pages[0].ResolvedSHA256 = member.ResolvedSHA256
	endorsed, err := PlanEndorsementPages(member.Member.ID, member.Resolved.Pages, member.Resolved, plan.Reservation.Numbers, recipe)
	require.NoError(t, err)
	plan.Pages[0].Layout = endorsed[0].Layout
	plan.Pages[0].Endorsements = endorsed[0].Endorsements
	plan, _, err = CanonicalRenderPlan(RenderPlan{Contract: RenderPlanContractV1, JobID: job.ID, RevisionSHA256: job.RevisionSHA256,
		Reservation: plan.Reservation, Pages: plan.Pages})
	require.NoError(t, err)
	engine, err := pdfproduction.NewPDFium(recipe)
	require.NoError(t, err)
	source := &syntheticProductionSource{data: pdf}
	handles := &syntheticPageHandles{}
	claim := JobClaim{JobID: job.ID, Token: "synthetic claim"}
	require.NoError(t, RenderProductionPages(t.Context(), source, handles, engine, claim, job, finalized, plan, recipe))
	require.Equal(t, 1, source.opens)
	sequence, err := NewVerifiedProductionPageSequence(t.Context(), handles, job, plan, *member, recipe.MaxStagingBytes)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sequence.Close()) })
	artifact, err := sequence.Next(t.Context())
	require.NoError(t, err)
	reader, err := artifact.OpenPNG()
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.NoError(t, engine.Close())
	fresh, err := WriteAndVerifyProductionMember(t.Context(), handles, job, plan, *member, recipe)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	require.Positive(t, fresh.Size)
	require.NotEmpty(t, fresh.SHA256)

	handles.stageErr = ErrJobStaleClaim
	handles.stage = ProductionPageStage{}
	engine, err = pdfproduction.NewPDFium(recipe)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	err = RenderProductionPages(t.Context(), source, handles, engine, claim, job, finalized, plan, recipe)
	require.ErrorIs(t, err, ErrJobStaleClaim)
	changed := plan
	changed.Pages = append([]RenderPagePlan(nil), plan.Pages...)
	changed.Pages[0].Endorsements = []redaction.Endorsement{{Text: "changed"}}
	changed, _, err = CanonicalRenderPlan(RenderPlan{Contract: changed.Contract, JobID: changed.JobID,
		RevisionSHA256: changed.RevisionSHA256, Reservation: changed.Reservation, Pages: changed.Pages})
	require.NoError(t, err)
	priorOpens := source.opens
	err = RenderProductionPages(t.Context(), source, handles, engine, claim, job, finalized, changed, recipe)
	require.ErrorIs(t, err, ErrJobConflict)
	require.Equal(t, priorOpens, source.opens)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = RenderProductionPages(ctx, source, handles, engine, claim, job, finalized, plan, recipe)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, priorOpens, source.opens)
	err = renderProductionPagesWithTimeout(t.Context(), source, handles, waitingProductionEngine{},
		claim, job, finalized, plan, recipe, 5*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRenderProductionPagesTwoPageMasksAndFreshVerification(t *testing.T) {
	fixture := newStoredFixture(t)
	finalized := fixture.finalized
	member := &finalized.Authority.Prepared.Members[0]
	recipe := pdfproduction.QualifiedRecipe()
	pageOne := redaction.Page{Number: 1, FrameSHA256: testHash("first frame"), Width: 85_000,
		Height: 110_000, Span: redaction.Span{Start: 0, End: 1}}
	pageTwo := redaction.Page{Number: 2, FrameSHA256: testHash("second frame"), Width: 85_000,
		Height: 110_000, Span: redaction.Span{Start: 1, End: 2}}
	member.Resolved = endorsementResolved(t, recipe, []redaction.Page{pageOne, pageTwo}, []endorsementDecision{{
		id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", label: "PUBLIC", page: 2,
		x0: 1_000, y0: 1_000, x1: 10_000, y1: 5_000,
	}})
	member.ResolvedSHA256 = member.Resolved.SHA256
	require.Len(t, member.Resolved.RedactBoxes, 1)
	require.Equal(t, 2, member.Resolved.RedactBoxes[0].Page)

	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetFont("Helvetica", "", 12)
	for _, value := range []string{"synthetic first page", "synthetic second page"} {
		pdf.AddPage()
		pdf.Text(72, 72, value)
	}
	var sourcePDF bytes.Buffer
	require.NoError(t, pdf.Output(&sourcePDF))
	member.Member.PDFSHA256 = testHash(sourcePDF.String())
	member.Member.PDFSize = int64(sourcePDF.Len())
	const jobID = "77000000-0000-4000-8000-000000000001"
	job := Job{ID: jobID, SetID: finalized.Draft.SetID, Revision: finalized.Draft.Revision,
		RevisionSHA256: finalized.Authority.Prepared.SHA256, PreparedInputSHA256: finalized.Authority.Receipt.SHA256}
	reservation := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1,
		Authority: "bates-ledger/v1", ID: "77000000-0000-4000-8000-000000000002", OperationID: jobID,
		RevisionSHA256: job.RevisionSHA256, State: "reserved", Numbers: []documentproduction.AssignedNumber{
			{MemberID: member.Member.ID, MemberOrdinal: 1, Page: 1, Text: "SYN000001"},
			{MemberID: member.Member.ID, MemberOrdinal: 1, Page: 2, Text: "SYN000002"},
		}}
	_, digest, err := documentproduction.CanonicalNumberReservation(reservation)
	require.NoError(t, err)
	reservation.SHA256 = digest
	job.Reservation = reservation
	endorsements, err := PlanEndorsementPages(member.Member.ID, member.Resolved.Pages, member.Resolved,
		reservation.Numbers, recipe)
	require.NoError(t, err)
	pages := make([]RenderPagePlan, 2)
	for index, endorsed := range endorsements {
		pages[index] = RenderPagePlan{MemberID: member.Member.ID, MemberOrdinal: 1, Page: index + 1,
			ResolvedSHA256: member.ResolvedSHA256, Layout: endorsed.Layout, Endorsements: endorsed.Endorsements}
	}
	plan, _, err := CanonicalRenderPlan(RenderPlan{Contract: RenderPlanContractV1, JobID: jobID,
		RevisionSHA256: job.RevisionSHA256, Reservation: reservation, Pages: pages})
	require.NoError(t, err)
	engine, err := pdfproduction.NewPDFium(recipe)
	require.NoError(t, err)
	source := &syntheticProductionSource{data: sourcePDF.Bytes()}
	archive := &syntheticPageArchive{}
	claim := JobClaim{JobID: jobID, Token: "synthetic two-page claim"}
	renderErr := RenderProductionPages(t.Context(), source, archive, engine, claim, job, finalized, plan, recipe)
	require.NoError(t, engine.Close())
	require.NoError(t, renderErr)
	require.Len(t, archive.pages, 2)
	require.NotEqual(t, archive.pages[1].stage.SHA256, archive.pages[2].stage.SHA256)
	require.NotEqual(t, archive.pages[1].stage.Artifact.SHA256, archive.pages[2].stage.Artifact.SHA256)
	fresh, err := WriteAndVerifyProductionMember(t.Context(), archive, job, plan, *member, recipe)
	require.NoError(t, err)
	require.NoError(t, fresh.Close())
	require.Equal(t, 2, archive.pages[1].openCount)
	require.Equal(t, 2, archive.pages[2].openCount)
	testPublishVerifiedProductionFromPages(t, finalized, job, plan, archive, recipe)
}

func (r *syntheticVerifiedPDF) Close() error { r.closed = true; return nil }
func (r *syntheticVerifiedPDF) Verify() error {
	if !r.verified {
		return errors.New("synthetic integrity failure")
	}
	return nil
}
func (r *syntheticVerifiedPDF) Verified() bool { return r.verified }

func syntheticPinnedPDF(data []byte, reader packstore.VerifiedReadCloser, size int64) PinnedProductionPDF {
	digest := sha256.Sum256(data)
	return PinnedProductionPDF{PDFSHA256: hex.EncodeToString(digest[:]), Size: size, Stream: reader}
}

func TestProductionSourceSpoolVerifiesBeforeReaderAt(t *testing.T) {
	data := []byte("%PDF-1.7\nsynthetic source\n")
	bad := &syntheticVerifiedPDF{Reader: bytes.NewReader(data)}
	spool, err := SpoolProductionPDF(t.Context(), syntheticPinnedPDF(data, bad, int64(len(data))), 1<<20)
	require.Error(t, err)
	require.Nil(t, spool)
	require.True(t, bad.closed)

	good := &syntheticVerifiedPDF{Reader: bytes.NewReader(data), verified: true}
	spool, err = SpoolProductionPDF(t.Context(), syntheticPinnedPDF(data, good, int64(len(data))), 1<<20)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.Close()) })
	require.True(t, good.closed)
	read := make([]byte, len(data))
	_, err = spool.File.ReadAt(read, 0)
	require.NoError(t, err)
	require.Equal(t, data, read)
	_, err = spool.File.Seek(0, io.SeekStart)
	require.NoError(t, err)

	tooLarge := &syntheticVerifiedPDF{Reader: bytes.NewReader(data), verified: true}
	spool, err = SpoolProductionPDF(context.Background(), syntheticPinnedPDF(data, tooLarge, int64(len(data))), 4)
	require.Error(t, err)
	require.Nil(t, spool)
	_, err = SpoolProductionPDF(t.Context(), syntheticPinnedPDF(data, &syntheticVerifiedPDF{Reader: bytes.NewReader(data), verified: true}, int64(len(data))+1), 1<<20)
	require.Error(t, err)
}
