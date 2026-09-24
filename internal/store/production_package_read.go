package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

// LoadProductionPackageInputs reads historical output authority. It deliberately
// avoids LoadFinalizedProduction, which checks that source heads are current for
// new rendering. Published output must remain packageable after source changes.
func (s *Store) LoadProductionPackageInputs(ctx context.Context, jobID string) (production.PublishedPackageInputs, error) {
	bad := func() (production.PublishedPackageInputs, error) {
		return production.PublishedPackageInputs{}, production.ErrJobConflict
	}
	job, err := s.LoadProductionJob(ctx, jobID)
	if err != nil {
		return production.PublishedPackageInputs{}, err
	}
	if job.State != production.ProductionJobSucceeded {
		return bad()
	}
	plan, err := s.LoadProductionRenderPlan(ctx, jobID)
	if err != nil || plan.RevisionSHA256 != job.RevisionSHA256 ||
		plan.Reservation.SHA256 != job.Receipt.NumberReservationSHA256 ||
		plan.LayoutSHA256 != job.Receipt.LayoutSHA256 {
		return bad()
	}
	const maxFrozenAuthorityBytes = 128 << 20
	var size int64
	var preparedSHA, receiptSHA string
	err = s.db.QueryRowContext(ctx, `SELECT length(prepared_input_json),prepared_sha256,prepared_input_sha256
		FROM production_finalized_revisions WHERE set_id=? AND revision=?`, job.SetID, job.Revision).
		Scan(&size, &preparedSHA, &receiptSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return bad()
	}
	if err != nil {
		return production.PublishedPackageInputs{}, err
	}
	if size < 1 || size > maxFrozenAuthorityBytes {
		return bad()
	}
	var raw []byte
	err = s.db.QueryRowContext(ctx, `SELECT prepared_input_json FROM production_finalized_revisions
		WHERE set_id=? AND revision=?`, job.SetID, job.Revision).Scan(&raw)
	if err != nil || len(raw) < 1 || len(raw) > maxFrozenAuthorityBytes {
		return bad()
	}
	authority, err := canonical.Decode[documentproduction.PreparedInputAuthority](raw)
	if err != nil || documentproduction.ValidatePreparedInputAuthority(authority) != nil || authority.Receipt == nil {
		return bad()
	}
	canonicalRaw, err := canonical.Marshal(authority)
	if err != nil || !bytes.Equal(raw, canonicalRaw) ||
		authority.Prepared.SetID != job.SetID || authority.Prepared.Revision != job.Revision ||
		authority.Prepared.SHA256 != preparedSHA || authority.Prepared.SHA256 != job.RevisionSHA256 ||
		authority.Receipt.SHA256 != receiptSHA || receiptSHA != job.PreparedInputSHA256 ||
		authority.Prepared.Policy.SHA256 != job.Receipt.PolicySHA256 {
		return bad()
	}
	members := make([]production.PackageMember, len(authority.Prepared.Members))
	for index, prepared := range authority.Prepared.Members {
		member := prepared.Member
		if member.Ordinal != int64(index+1) || member.ID == "" || member.Family.RootVersionID == "" {
			return bad()
		}
		members[index] = production.PackageMember{ID: member.ID, Ordinal: member.Ordinal, FamilyID: member.Family.RootVersionID}
	}
	return production.PublishedPackageInputs{Job: job, Reservation: plan.Reservation, Members: members}, nil
}
