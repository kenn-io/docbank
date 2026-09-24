package store

import (
	"context"
	"database/sql"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

// PublishedProductionNumber binds a ledger label to one published occurrence
// and an output artifact with catalog authority. The path is presentation metadata;
// neither the path nor a Bates-looking filename is used to find the artifact.
type PublishedProductionNumber struct {
	Label                   string
	JobID                   string
	SetID                   string
	Revision                int64
	ProductionReceiptSHA256 string
	ArtifactManifestSHA256  string
	SourceVersionID         string
	OccurrenceID            string
	Page                    int
	ArtifactID              string
	ArtifactSHA256          string
	ArtifactPath            string
	Volume                  string
}

// FindPublishedProductionNumber resolves one exact label through the existing
// Bates ledger and then checks the frozen production, reservation and output
// authorities. Labels belonging only to an export or an unpublished job do not
// resolve as production output.
func (s *Store) FindPublishedProductionNumber(ctx context.Context, label string) (PublishedProductionNumber, error) {
	if ctx == nil || label == "" || len(label) > 256 {
		return PublishedProductionNumber{}, ErrInvalidBatesSelector
	}
	var jobID, allocationID string
	err := s.db.QueryRowContext(ctx, `SELECT j.job_id,l.allocation_id FROM bates_page_labels l
		JOIN bates_allocations a USING(allocation_id)
		JOIN production_jobs j ON j.job_id=a.operation_id AND j.state='succeeded'
		WHERE l.label=?`, label).Scan(&jobID, &allocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedProductionNumber{}, ErrNotFound
	}
	if err != nil {
		return PublishedProductionNumber{}, err
	}
	bad := func() (PublishedProductionNumber, error) {
		return PublishedProductionNumber{}, production.ErrJobConflict
	}
	job, err := s.LoadProductionJob(ctx, jobID)
	if err != nil || job.State != production.ProductionJobSucceeded {
		return bad()
	}
	plan, err := s.LoadProductionRenderPlan(ctx, jobID)
	if err != nil || plan.Reservation.ID != allocationID ||
		plan.Reservation.SHA256 != job.Receipt.NumberReservationSHA256 ||
		plan.RevisionSHA256 != job.RevisionSHA256 {
		return bad()
	}
	allocation, err := s.ProductionNumberingForJob(ctx, jobID)
	if err != nil || allocation.AllocationID != allocationID ||
		allocation.State != batesAllocationStateReserved && allocation.State != batesAllocationStateCommitted {
		return bad()
	}
	var number documentproduction.AssignedNumber
	var pageOrdinal int
	var count int
	for index, candidate := range plan.Reservation.Numbers {
		if candidate.Text == label {
			number = candidate
			pageOrdinal = index + 1
			count++
		}
	}
	if count != 1 {
		return bad()
	}
	ledgerMatch := false
	for _, candidate := range allocation.Labels {
		if matchesPublishedProductionNumberLedger(candidate, number, pageOrdinal) {
			ledgerMatch = true
			break
		}
	}
	if !ledgerMatch {
		return bad()
	}
	var authoritySize int64
	err = s.db.QueryRowContext(ctx, `SELECT length(prepared_input_json) FROM production_finalized_revisions
		WHERE set_id=? AND revision=?`, job.SetID, job.Revision).Scan(&authoritySize)
	if err != nil || authoritySize < 1 || authoritySize > 128<<20 {
		return bad()
	}
	var raw []byte
	var preparedSHA, preparedInputSHA string
	err = s.db.QueryRowContext(ctx, `SELECT prepared_input_json,prepared_sha256,prepared_input_sha256
		FROM production_finalized_revisions WHERE set_id=? AND revision=?`, job.SetID, job.Revision).
		Scan(&raw, &preparedSHA, &preparedInputSHA)
	if err != nil || len(raw) != int(authoritySize) || preparedSHA != job.RevisionSHA256 ||
		preparedInputSHA != job.PreparedInputSHA256 {
		return bad()
	}
	authority, err := canonical.Decode[documentproduction.PreparedInputAuthority](raw)
	if err != nil || documentproduction.ValidatePreparedInputAuthority(authority) != nil ||
		authority.Prepared.SHA256 != preparedSHA || authority.Receipt == nil ||
		authority.Receipt.SHA256 != preparedInputSHA || authority.Prepared.SetID != job.SetID ||
		authority.Prepared.Revision != job.Revision {
		return bad()
	}
	var sourceVersionID string
	var sourceNodeID int64
	var sourceSHA256 string
	var sourceSize int64
	for _, prepared := range authority.Prepared.Members {
		if prepared.Member.ID == number.MemberID && prepared.Member.Ordinal == number.MemberOrdinal {
			if sourceVersionID != "" {
				return bad()
			}
			sourceVersionID = prepared.Member.SourceVersionID
			sourceNodeID = prepared.Member.NodeID
			sourceSHA256 = prepared.Member.SourceSHA256
			sourceSize = prepared.Member.SourceSize
		}
	}
	if sourceVersionID == "" {
		return bad()
	}
	sourceVersion, err := s.ContentVersionByID(ctx, sourceVersionID)
	if err != nil || sourceVersion.NodeID != sourceNodeID || sourceVersion.BlobHash != sourceSHA256 ||
		sourceVersion.Size != sourceSize {
		return bad()
	}
	var artifact documentproduction.Artifact
	for _, candidate := range job.Manifest.Artifacts {
		if candidate.MemberID == number.MemberID && candidate.MemberOrdinal == number.MemberOrdinal &&
			candidate.Role == documentproduction.ArtifactRoleRedactedPDF {
			if artifact.ID != "" {
				return bad()
			}
			artifact = candidate
		}
	}
	if artifact.ID == "" {
		return bad()
	}
	stored, err := s.LoadProductionJobArtifact(ctx, jobID, artifact.ID)
	if err != nil || stored != artifact {
		return bad()
	}
	return PublishedProductionNumber{Label: label, JobID: job.ID, SetID: job.SetID,
		Revision: job.Revision, ProductionReceiptSHA256: job.Receipt.SHA256,
		ArtifactManifestSHA256: job.Manifest.SHA256, SourceVersionID: sourceVersionID,
		OccurrenceID: number.MemberID, Page: number.Page, ArtifactID: artifact.ID,
		ArtifactSHA256: artifact.SHA256, ArtifactPath: artifact.Path, Volume: artifact.Volume}, nil
}

func matchesPublishedProductionNumberLedger(label BatesPageLabel, number documentproduction.AssignedNumber,
	pageOrdinal int) bool {
	return label.Label == number.Text && label.OccurrenceID == number.MemberID &&
		label.Ordinal == pageOrdinal && label.SourcePage == number.Page
}

// PublishedProductionNumberPage continues a numeric range within one Bates
// namespace. NextSequence is zero after the last published production number.
type PublishedProductionNumberPage struct {
	Items        []PublishedProductionNumber
	NextSequence int64
}

// FindPublishedProductionNumberRange returns up to 25 ledger-ordered published
// production numbers. Every item passes the same checks as an exact lookup.
func (s *Store) FindPublishedProductionNumberRange(ctx context.Context,
	namespaceID string, startSequence, endSequence, afterSequence int64, limit int,
) (PublishedProductionNumberPage, error) {
	if ctx == nil || validateUUIDv4(namespaceID) != nil || startSequence < 1 ||
		endSequence < startSequence || afterSequence < 0 || afterSequence > endSequence ||
		limit < 1 || limit > 25 {
		return PublishedProductionNumberPage{}, ErrInvalidBatesSelector
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.label,l.sequence FROM bates_page_labels l
		JOIN bates_allocations a USING(allocation_id)
		JOIN production_jobs j ON j.job_id=a.operation_id AND j.state='succeeded'
		WHERE l.namespace_id=? AND l.sequence>=? AND l.sequence<=? AND l.sequence>?
		ORDER BY l.sequence LIMIT ?`, namespaceID, startSequence, endSequence, afterSequence, limit+1)
	if err != nil {
		return PublishedProductionNumberPage{}, err
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		label    string
		sequence int64
	}
	labels := make([]row, 0, limit+1)
	for rows.Next() {
		var label row
		if err := rows.Scan(&label.label, &label.sequence); err != nil {
			return PublishedProductionNumberPage{}, err
		}
		labels = append(labels, label)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return PublishedProductionNumberPage{}, err
	}
	page := PublishedProductionNumberPage{Items: make([]PublishedProductionNumber, 0, limit)}
	if len(labels) > limit {
		labels = labels[:limit]
		page.NextSequence = labels[len(labels)-1].sequence
	}
	for _, label := range labels {
		match, err := s.FindPublishedProductionNumber(ctx, label.label)
		if err != nil {
			return PublishedProductionNumberPage{}, err
		}
		page.Items = append(page.Items, match)
	}
	return page, nil
}
