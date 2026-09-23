package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/kit/packstore"
)

// LoadFinalizedProduction reads J's immutable finalized-revision authority.
// A passing prepared-input gate is not a finalized draft and is never promoted
// into one here. The coordinator schema checkpoint supplies this table.
func (s *Store) LoadFinalizedProduction(ctx context.Context, setID string, revision int64) (productionservice.FinalizedProduction, error) {
	var draftRaw, authorityRaw []byte
	err := s.db.QueryRowContext(ctx, `SELECT draft_json,prepared_input_json
		FROM production_finalized_revisions WHERE set_id=? AND revision=?`, setID, revision).Scan(&draftRaw, &authorityRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return productionservice.FinalizedProduction{}, ErrNotFound
	}
	if err != nil {
		return productionservice.FinalizedProduction{}, err
	}
	draft, err := canonical.Decode[redaction.Draft](draftRaw)
	if err != nil {
		return productionservice.FinalizedProduction{}, err
	}
	authority, err := canonical.Decode[documentproduction.PreparedInputAuthority](authorityRaw)
	if err != nil {
		return productionservice.FinalizedProduction{}, err
	}
	if draft.State != "finalized" || draft.SetID != setID || draft.Revision != revision {
		return productionservice.FinalizedProduction{}, ErrPackageConflict
	}
	if err := redaction.ValidateDraft(draft); err != nil {
		return productionservice.FinalizedProduction{}, err
	}
	if err := documentproduction.ValidatePreparedInputAuthority(authority); err != nil {
		return productionservice.FinalizedProduction{}, err
	}
	if authority.Prepared.SetID != draft.SetID || authority.Prepared.Revision != draft.Revision || authority.Prepared.ETag != draft.ETag || authority.Prepared.MemberHash != draft.MemberHash || authority.Prepared.DecisionsSHA256 != draft.DecisionsSHA256 {
		return productionservice.FinalizedProduction{}, ErrPackageConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return productionservice.FinalizedProduction{}, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, prepared := range authority.Prepared.Members {
		if err := s.validateFinalizedProductionMemberTx(ctx, tx, prepared); err != nil {
			return productionservice.FinalizedProduction{}, ErrInvalidProduction
		}
	}
	if err := tx.Commit(); err != nil {
		return productionservice.FinalizedProduction{}, ErrInvalidProduction
	}
	return productionservice.FinalizedProduction{Draft: draft, Authority: authority}, nil
}

func (s *Store) validateFinalizedProductionMemberTx(ctx context.Context, tx *sql.Tx, prepared documentproduction.PreparedMember) error {
	member := prepared.Member
	if member.VaultID != s.vaultID {
		return ErrInvalidProduction
	}
	mapAuthority, found, err := loadProductionTextMapTx(ctx, tx, member.MapSHA256)
	if err != nil || !found || mapAuthority.SourceNodeID != member.NodeID ||
		mapAuthority.Source.VersionID != member.SourceVersionID || mapAuthority.Source.SHA256 != member.SourceSHA256 ||
		mapAuthority.Source.Size != member.SourceSize || mapAuthority.PDFSHA256 != member.PDFSHA256 ||
		mapAuthority.PDFSize != member.PDFSize || !slices.Equal(mapAuthority.Map.Pages, prepared.Resolved.Pages) {
		return ErrInvalidProduction
	}
	pageInventory, err := productionPageInventorySHA256(mapAuthority.Map.Pages)
	if err != nil || pageInventory != member.PageInventorySHA256 {
		return ErrInvalidProduction
	}
	if len(prepared.Frames) != len(mapAuthority.Map.Pages) {
		return ErrInvalidProduction
	}
	for index, page := range mapAuthority.Map.Pages {
		frame := prepared.Frames[index]
		if frame.Page != page.Number || frame.SHA256 != page.FrameSHA256 || frame.Width != page.Width || frame.Height != page.Height {
			return ErrInvalidProduction
		}
	}
	var currentVersion string
	if err := tx.QueryRowContext(ctx, `SELECT current_version_id FROM nodes WHERE id=?`, member.NodeID).Scan(&currentVersion); err != nil || currentVersion != member.SourceVersionID {
		return ErrInvalidProduction
	}
	return nil
}

// FinalizedProductionPDF is a private, catalog-authorized stream handle.
// The consumer must read through EOF or call Verify before using its bytes.
type FinalizedProductionPDF struct {
	SourceVersionID, PDFSHA256, RenditionAttachmentID, RenditionBuildID, RenditionArtifactID string
	Size                                                                                     int64
	Stream                                                                                   packstore.VerifiedReadCloser
}

// OpenFinalizedProductionPDF takes no caller-supplied hash. It reloads the
// sealed occurrence and its exact source/map/rendition relation first.
func (s *Store) OpenFinalizedProductionPDF(ctx context.Context, setID string, revision int64, memberID string, opener RenditionBlobReader) (FinalizedProductionPDF, error) {
	if opener == nil || memberID == "" {
		return FinalizedProductionPDF{}, ErrInvalidProduction
	}
	finalized, err := s.LoadFinalizedProduction(ctx, setID, revision)
	if err != nil {
		return FinalizedProductionPDF{}, err
	}
	for _, prepared := range finalized.Authority.Prepared.Members {
		member := prepared.Member
		if member.ID != memberID {
			continue
		}
		var mapAuthority ProductionTextMapAuthority
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return FinalizedProductionPDF{}, err
		}
		mapAuthority, found, loadErr := loadProductionTextMapTx(ctx, tx, member.MapSHA256)
		if loadErr != nil || !found || s.validateFinalizedProductionMemberTx(ctx, tx, prepared) != nil || tx.Commit() != nil {
			_ = tx.Rollback()
			return FinalizedProductionPDF{}, ErrInvalidProduction
		}
		stream, size, err := opener.OpenStreamContext(ctx, member.PDFSHA256)
		if err != nil {
			if stream != nil {
				_ = stream.Close()
			}
			return FinalizedProductionPDF{}, ErrInvalidProduction
		}
		if stream == nil {
			return FinalizedProductionPDF{}, ErrInvalidProduction
		}
		if size != member.PDFSize {
			_ = stream.Close()
			return FinalizedProductionPDF{}, ErrInvalidProduction
		}
		return FinalizedProductionPDF{SourceVersionID: member.SourceVersionID, PDFSHA256: member.PDFSHA256,
			RenditionAttachmentID: mapAuthority.RenditionAttachmentID, RenditionBuildID: mapAuthority.RenditionBuildID,
			RenditionArtifactID: mapAuthority.RenditionArtifactID, Size: size, Stream: stream}, nil
	}
	return FinalizedProductionPDF{}, ErrInvalidProduction
}

type productionSnapshotFields struct {
	MapSHA256           string `json:"production_map_sha256"`
	PageInventorySHA256 string `json:"page_inventory_sha256"`
}

// SealProductionNumberingSnapshot freezes one Bates occurrence per prepared
// member. Production deliberately permits the same source version more than
// once; ordinary collection snapshots retain their duplicate-node rule.
func (s *Store) SealProductionNumberingSnapshot(ctx context.Context, snapshotID, gateOperationID string) (CollectionSnapshot, error) {
	authority, err := s.PreparedInputAuthority(ctx, gateOperationID)
	if err != nil || authority.Receipt == nil || !documentproduction.GateResultsPassed(authority.GateResults) {
		return CollectionSnapshot{}, errors.Join(ErrInvalidProduction, err)
	}
	members := make([]CollectionSnapshotMember, len(authority.Prepared.Members))
	for index, prepared := range authority.Prepared.Members {
		member := prepared.Member
		pages := make([]int, len(prepared.Resolved.Pages))
		for pageIndex, page := range prepared.Resolved.Pages {
			pages[pageIndex] = page.Number
		}
		if len(pages) == 0 || len(prepared.Frames) == 0 {
			return CollectionSnapshot{}, ErrInvalidProduction
		}
		fields, err := canonical.Marshal(productionSnapshotFields{member.MapSHA256, member.PageInventorySHA256})
		if err != nil {
			return CollectionSnapshot{}, err
		}
		members[index] = CollectionSnapshotMember{
			Ordinal: index + 1, OccurrenceID: member.ID, NodeID: member.NodeID,
			ContentVersionID: member.SourceVersionID, BlobSHA256: member.SourceSHA256,
			Size: member.SourceSize, FamilyID: member.ID, FamilyOrder: 1,
			DisplayName:      fmt.Sprintf("Production occurrence %d", index+1),
			FrozenFieldsJSON: string(fields), DocumentKind: "production",
			SelectedSourcePages: pages, SelectedPDFSHA256: member.PDFSHA256,
			SourcePageCount: len(prepared.Frames),
		}
	}
	request := SnapshotSealRequest{SnapshotID: snapshotID, Members: members}
	return s.sealCollectionSnapshot(ctx, request, true, maxProductionSnapshotManifest, func(ctx context.Context, tx *sql.Tx, request SnapshotSealRequest) error {
		for index, member := range request.Members {
			if !productionSnapshotPDFValid(ctx, tx, member) {
				return ErrInvalidProduction
			}
			mapAuthority, found, err := loadProductionTextMapTx(ctx, tx, authority.Prepared.Members[index].Member.MapSHA256)
			if err != nil || !found || mapAuthority.Source.VersionID != member.ContentVersionID ||
				mapAuthority.PDFSHA256 != member.SelectedPDFSHA256 ||
				mapAuthority.PDFSize != authority.Prepared.Members[index].Member.PDFSize ||
				len(mapAuthority.Map.Pages) != member.SourcePageCount {
				return errors.Join(ErrInvalidProduction, err)
			}
		}
		return nil
	})
}

func productionSnapshotPDFValid(ctx context.Context, q metadataQuerier, member CollectionSnapshotMember) bool {
	if member.DocumentKind != "production" {
		return false
	}
	fields, err := canonical.Decode[productionSnapshotFields]([]byte(member.FrozenFieldsJSON))
	if err != nil || !canonical.IsSHA256Hex(fields.MapSHA256) || !canonical.IsSHA256Hex(fields.PageInventorySHA256) {
		return false
	}
	var count int
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM production_text_maps m
		JOIN content_versions v ON v.version_id=m.version_id AND v.node_id=m.source_node_id
		WHERE m.map_sha256=? AND m.page_inventory_sha256=? AND m.source_node_id=?
		AND m.version_id=? AND m.source_sha256=? AND m.source_size=?
		AND m.pdf_sha256=? AND v.blob_hash=? AND v.size=?`,
		fields.MapSHA256, fields.PageInventorySHA256, member.NodeID,
		member.ContentVersionID, member.BlobSHA256, member.Size,
		member.SelectedPDFSHA256, member.BlobSHA256, member.Size).Scan(&count)
	return err == nil && count == 1
}

func validateProductionNumberingSnapshotTx(ctx context.Context, tx *sql.Tx, snapshotID string,
	prepared documentproduction.PreparedProduction) error {
	snapshot, err := loadCollectionSnapshotTx(ctx, tx, snapshotID)
	if err != nil || snapshot.MemberCount != len(prepared.Members) || snapshot.PageCount == 0 {
		return errors.Join(ErrInvalidProduction, err)
	}
	pages, err := expectedBatesPagesLimited(ctx, tx, snapshotID, MaxSnapshotPages)
	if err != nil {
		return errors.Join(ErrInvalidProduction, err)
	}
	members, err := loadSnapshotMemberRows(ctx, tx, snapshotID, 0, snapshot.MemberCount)
	if err != nil || len(members) != len(prepared.Members) {
		return errors.Join(ErrInvalidProduction, err)
	}
	pageIndex := 0
	for index, expected := range prepared.Members {
		row, member := members[index], expected.Member
		fields, err := canonical.Decode[productionSnapshotFields]([]byte(row.FrozenFieldsJSON))
		if err != nil || row.Ordinal != index+1 || row.OccurrenceID != member.ID ||
			row.DocumentKind != "production" || row.FamilyID != member.ID || row.FamilyOrder != 1 ||
			row.ParentOccurrenceID != "" ||
			row.NodeID != member.NodeID || row.ContentVersionID != member.SourceVersionID ||
			row.BlobSHA256 != member.SourceSHA256 || row.Size != member.SourceSize ||
			row.SelectedPDFSHA256 != member.PDFSHA256 || row.SourcePageCount != len(expected.Frames) ||
			fields.MapSHA256 != member.MapSHA256 || fields.PageInventorySHA256 != member.PageInventorySHA256 ||
			len(row.SelectedSourcePages) != len(expected.Resolved.Pages) {
			return ErrInvalidProduction
		}
		for offset, page := range expected.Resolved.Pages {
			if row.SelectedSourcePages[offset] != page.Number || pageIndex >= len(pages) ||
				pages[pageIndex] != (BatesPageInput{OccurrenceID: member.ID,
					UnstampedSHA256: member.PDFSHA256, SourcePage: page.Number,
					VerifiedPageCount: row.SourcePageCount}) {
				return ErrInvalidProduction
			}
			pageIndex++
		}
	}
	if pageIndex != len(pages) {
		return ErrInvalidProduction
	}
	return nil
}

// FinalizeProductionRevision atomically seals the exact finalized draft,
// prepared authority, and numbering snapshot. Replays must be byte-identical.
func (s *Store) FinalizeProductionRevision(ctx context.Context, r productionservice.FinalizationRequest) error {
	f := r.Finalized
	if f.Draft.State != "finalized" || f.Draft.SetID == "" || validateUUIDv4(r.OperationID) != nil || r.NamespaceID == "" || r.SnapshotID == "" || !canonical.IsSHA256Hex(r.RecipeSHA256) {
		return productionservice.ErrJobConflict
	}
	if err := redaction.ValidateDraft(f.Draft); err != nil {
		return err
	}
	if err := documentproduction.ValidatePreparedInputAuthority(f.Authority); err != nil {
		return err
	}
	draftRaw, err := canonical.Marshal(f.Draft)
	if err != nil {
		return err
	}
	authorityRaw, err := canonical.Marshal(f.Authority)
	if err != nil {
		return err
	}
	draftSum := sha256.Sum256(draftRaw)
	draftSHA := hex.EncodeToString(draftSum[:])
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var storedRaw []byte
		var storedRequestSHA, storedResponseSHA string
		var stored documentproduction.PreparedInputAuthority
		if err := tx.QueryRowContext(ctx, `SELECT request_sha256,response_sha256,response_json FROM production_operation_receipts WHERE operation_id=? AND kind=?`, r.OperationID, productionOperationPreparedInputs).Scan(&storedRequestSHA, &storedResponseSHA, &storedRaw); err != nil {
			return err
		}
		stored, err = decodePreparedInputAuthority(storedRaw)
		if err != nil || productionSHA256(storedRaw) != storedResponseSHA ||
			stored.Audit.RequestSHA256 != storedRequestSHA || !bytes.Equal(storedRaw, authorityRaw) ||
			stored.Audit.OperationID != r.OperationID || !documentproduction.GateResultsPassed(stored.GateResults) {
			return productionservice.ErrJobConflict
		}
		var priorDraft, priorAuthority []byte
		var priorETag, priorStart int64
		var priorDraftSHA, priorPreparedSHA, priorGateSHA, priorNamespace, priorSnapshot, priorRecipe string
		err = tx.QueryRowContext(ctx, `SELECT etag,draft_sha256,prepared_sha256,prepared_input_sha256,draft_json,prepared_input_json,numbering_namespace_id,numbering_snapshot_id,numbering_recipe_sha256,numbering_start_at FROM production_finalized_revisions WHERE set_id=? AND revision=?`, f.Draft.SetID, f.Draft.Revision).Scan(&priorETag, &priorDraftSHA, &priorPreparedSHA, &priorGateSHA, &priorDraft, &priorAuthority, &priorNamespace, &priorSnapshot, &priorRecipe, &priorStart)
		if err == nil {
			if priorETag != f.Draft.ETag || priorDraftSHA != draftSHA || priorPreparedSHA != f.Authority.Prepared.SHA256 || priorGateSHA != f.Authority.Receipt.SHA256 || priorNamespace != r.NamespaceID || priorSnapshot != r.SnapshotID || priorRecipe != r.RecipeSHA256 || priorStart != r.StartAt || !bytes.Equal(priorDraft, draftRaw) || !bytes.Equal(priorAuthority, authorityRaw) {
				return productionservice.ErrJobConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		preparedAt, err := time.Parse(time.RFC3339Nano, stored.Prepared.PreparedAt)
		if err != nil {
			return productionservice.ErrJobConflict
		}
		current, err := s.LoadProductionGateSnapshot(ctx, tx, productionservice.PreparedInputRequest{
			SetID: f.Draft.SetID, Revision: f.Draft.Revision, PreparedAt: preparedAt,
		})
		if err != nil {
			return errors.Join(productionservice.ErrJobConflict, err)
		}
		if current.Policy.Approval.Required {
			if _, err := s.LoadProductionGateSnapshot(ctx, tx, productionservice.PreparedInputRequest{
				SetID: f.Draft.SetID, Revision: f.Draft.Revision, PreparedAt: time.Now().UTC(),
			}); err != nil {
				return errors.Join(productionservice.ErrJobConflict, err)
			}
		}
		expectedDraft := current.Draft
		expectedDraft.State = "finalized"
		currentRevisionSHA, err := productionservice.ProductionRevisionSHA256(current)
		if err != nil || expectedDraft != f.Draft {
			return errors.Join(productionservice.ErrJobConflict, err)
		}
		reconstructedRequestSHA, err := productionservice.PreparedInputRequestSHA256(productionservice.PreparedInputRequest{
			OperationID: r.OperationID, ReceiptID: stored.Receipt.ID,
			SetID: f.Draft.SetID, Revision: f.Draft.Revision,
			ExpectedETag: current.Draft.ETag, ExpectedRevisionSHA256: currentRevisionSHA,
			PreparedAt: preparedAt,
		})
		if err != nil || reconstructedRequestSHA != storedRequestSHA ||
			current.Draft.NumberingRecipeID != redaction.BatesNumberingRecipeID ||
			r.RecipeSHA256 != current.Draft.NumberingRecipeSHA256 || r.StartAt < 0 {
			return errors.Join(productionservice.ErrJobConflict, err)
		}
		if err := validateProductionNumberingSnapshotTx(ctx, tx, r.SnapshotID, stored.Prepared); err != nil {
			return errors.Join(productionservice.ErrJobConflict, err)
		}
		var namespaceCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bates_namespaces WHERE namespace_id=?`, r.NamespaceID).Scan(&namespaceCount); err != nil || namespaceCount != 1 {
			return errors.Join(productionservice.ErrJobConflict, err)
		}
		updated, err := tx.ExecContext(ctx, `UPDATE production_revisions SET state='finalized'
			WHERE set_id=? AND revision=? AND etag=? AND state='draft'`, f.Draft.SetID, f.Draft.Revision, f.Draft.ETag)
		if err != nil {
			return err
		}
		if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
			return errors.Join(productionservice.ErrJobConflict, err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_finalized_revisions(set_id,revision,etag,draft_sha256,prepared_sha256,prepared_input_sha256,draft_json,prepared_input_json,numbering_namespace_id,numbering_snapshot_id,numbering_recipe_sha256,numbering_start_at,finalized_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, f.Draft.SetID, f.Draft.Revision, f.Draft.ETag, draftSHA, f.Authority.Prepared.SHA256, f.Authority.Receipt.SHA256, draftRaw, authorityRaw, r.NamespaceID, r.SnapshotID, r.RecipeSHA256, r.StartAt, nowRFC3339())
		return err
	})
}

func (s *Store) AdmitProductionJob(ctx context.Context, r productionservice.JobRequest) (productionservice.Job, error) {
	if validateUUIDv4(r.JobID) != nil || validateUUIDv4(r.OperationID) != nil || validateUUIDv4(r.SetID) != nil || r.Revision < 1 || r.ETag < 1 || !canonical.IsSHA256Hex(r.PreparedInputSHA256) || !canonical.IsSHA256Hex(r.RevisionSHA256) || !canonical.IsSHA256Hex(r.NumberingProfileSHA256) {
		return productionservice.Job{}, ErrPackageConflict
	}
	var job productionservice.Job
	requestRaw, err := canonical.Marshal(r)
	if err != nil {
		return productionservice.Job{}, err
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var existingSHA, existingOperation, existingSet string
		var existingRevision, existingETag int64
		var existingRequest []byte
		var state string
		err := tx.QueryRowContext(ctx, `SELECT operation_id,set_id,revision,etag,prepared_input_sha256,request_json,state FROM production_jobs WHERE job_id=?`, r.JobID).Scan(&existingOperation, &existingSet, &existingRevision, &existingETag, &existingSHA, &existingRequest, &state)
		if err == nil {
			if existingOperation != r.OperationID || existingSet != r.SetID || existingRevision != r.Revision || existingETag != r.ETag || existingSHA != r.PreparedInputSHA256 || !bytes.Equal(existingRequest, requestRaw) {
				return ErrPackageConflict
			}
			job = productionservice.Job{ID: r.JobID, OperationID: r.OperationID, SetID: r.SetID, Revision: r.Revision, ETag: r.ETag, PreparedInputSHA256: existingSHA, RevisionSHA256: r.RevisionSHA256, State: state}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := nowRFC3339()
		_, err = tx.ExecContext(ctx, `INSERT INTO production_jobs(job_id,operation_id,owner,state,set_id,revision,etag,revision_sha256,prepared_input_sha256,receipt_sha256,request_json,claim_epoch,claim_token,claim_owner,cancel_requested,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,0,'','',0,?,?)`, r.JobID, r.OperationID, "production", productionservice.ProductionJobQueued, r.SetID, r.Revision, r.ETag, r.RevisionSHA256, r.PreparedInputSHA256, "", requestRaw, now, now)
		if s.driver.IsUniqueViolation(err) {
			return productionservice.ErrJobConflict
		}
		if err != nil {
			return err
		}
		job = productionservice.Job{ID: r.JobID, OperationID: r.OperationID, SetID: r.SetID, Revision: r.Revision, ETag: r.ETag, PreparedInputSHA256: r.PreparedInputSHA256, RevisionSHA256: r.RevisionSHA256, State: productionservice.ProductionJobQueued}
		return nil
	})
	return job, err
}

func (s *Store) RenewProductionJobClaim(ctx context.Context, claim productionservice.JobClaim, lease time.Duration) (productionservice.JobClaim, error) {
	if lease <= 0 {
		return productionservice.JobClaim{}, productionservice.ErrJobStaleClaim
	}
	until := time.Now().UTC().Add(lease).Format(time.RFC3339Nano)
	var next = claim
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var epoch int64
		var token, owner, state string
		if err := tx.QueryRowContext(ctx, `SELECT claim_epoch,claim_token,claim_owner,state FROM production_jobs WHERE job_id=?`, claim.JobID).Scan(&epoch, &token, &owner, &state); err != nil {
			return err
		}
		if epoch != claim.Epoch || token != claim.Token || owner != claim.Worker || state == productionservice.ProductionJobCanceled {
			return productionservice.ErrJobStaleClaim
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_jobs SET lease_expires_at=?,updated_at=? WHERE job_id=?`, until, nowRFC3339(), claim.JobID); err != nil {
			return err
		}
		next.ExpiresAt = time.Now().UTC().Add(lease)
		return nil
	})
	return next, err
}

// ClaimProductionJob fences workers with a monotonically increasing epoch and
// an opaque token. An expired claim is the only state a new worker may take.
func (s *Store) ClaimProductionJob(ctx context.Context, jobID, worker string, lease time.Duration) (productionservice.JobClaim, error) {
	if validateUUIDv4(jobID) != nil || worker == "" || lease <= 0 {
		return productionservice.JobClaim{}, productionservice.ErrJobConflict
	}
	var claim productionservice.JobClaim
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var state, token, owner string
		var epoch int64
		var expires sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT state,claim_epoch,claim_token,claim_owner,lease_expires_at FROM production_jobs WHERE job_id=?`, jobID).Scan(&state, &epoch, &token, &owner, &expires); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state == productionservice.ProductionJobCanceled || state == productionservice.ProductionJobSucceeded {
			return productionservice.ErrJobConflict
		}
		if expires.Valid {
			if t, err := time.Parse(time.RFC3339Nano, expires.String); err == nil && t.After(time.Now().UTC()) && owner != worker {
				return productionservice.ErrJobStaleClaim
			}
		}
		token, err := newUUIDv4()
		if err != nil {
			return err
		}
		epoch++
		until := time.Now().UTC().Add(lease).Format(time.RFC3339Nano)
		if _, err = tx.ExecContext(ctx, `UPDATE production_jobs SET state=?,claim_epoch=?,claim_token=?,claim_owner=?,lease_expires_at=?,updated_at=? WHERE job_id=?`, productionservice.ProductionJobRunning, epoch, token, worker, until, nowRFC3339(), jobID); err != nil {
			return err
		}
		claim = productionservice.JobClaim{JobID: jobID, Worker: worker, Token: token, Epoch: epoch, ExpiresAt: time.Now().UTC().Add(lease)}
		return nil
	})
	return claim, err
}

// ReserveProductionJobNumbers resolves the sealed numbering authority recorded
// with the finalized revision and delegates allocation to the existing Bates
// ledger. Caller-supplied allocation IDs are never accepted as authority.
func (s *Store) ReserveProductionJobNumbers(ctx context.Context, job productionservice.Job, finalized productionservice.FinalizedProduction) (documentproduction.NumberReservation, error) {
	stored, err := s.LoadFinalizedProduction(ctx, finalized.Draft.SetID, finalized.Draft.Revision)
	if err != nil {
		return documentproduction.NumberReservation{}, err
	}
	want, err := canonical.Marshal(stored)
	if err != nil {
		return documentproduction.NumberReservation{}, err
	}
	got, err := canonical.Marshal(finalized)
	if err != nil || !bytes.Equal(want, got) || stored.Authority.Receipt == nil ||
		job.SetID != stored.Draft.SetID || job.Revision != stored.Draft.Revision ||
		job.ETag != stored.Draft.ETag || job.RevisionSHA256 != stored.Authority.Prepared.SHA256 ||
		job.PreparedInputSHA256 != stored.Authority.Receipt.SHA256 {
		return documentproduction.NumberReservation{}, errors.Join(productionservice.ErrJobConflict, err)
	}
	var admittedSetID, admittedPreparedSHA, admittedRevisionSHA, state string
	var admittedRevision, admittedETag int64
	err = s.db.QueryRowContext(ctx, `SELECT set_id,revision,etag,prepared_input_sha256,revision_sha256,state
		FROM production_jobs WHERE job_id=?`, job.ID).Scan(&admittedSetID, &admittedRevision,
		&admittedETag, &admittedPreparedSHA, &admittedRevisionSHA, &state)
	if err != nil || admittedSetID != job.SetID || admittedRevision != job.Revision ||
		admittedETag != job.ETag || admittedPreparedSHA != job.PreparedInputSHA256 ||
		admittedRevisionSHA != job.RevisionSHA256 || state == productionservice.ProductionJobCanceled {
		return documentproduction.NumberReservation{}, errors.Join(productionservice.ErrJobConflict, err)
	}
	var namespaceID, snapshotID, recipeSHA string
	var startAt int64
	err = s.db.QueryRowContext(ctx, `SELECT numbering_namespace_id,numbering_snapshot_id,numbering_recipe_sha256,numbering_start_at FROM production_finalized_revisions WHERE set_id=? AND revision=?`, finalized.Draft.SetID, finalized.Draft.Revision).Scan(&namespaceID, &snapshotID, &recipeSHA, &startAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return documentproduction.NumberReservation{}, ErrNotFound
		}
		return documentproduction.NumberReservation{}, err
	}
	pages, err := s.SnapshotBatesPages(ctx, snapshotID, MaxSnapshotPages)
	if err != nil {
		return documentproduction.NumberReservation{}, err
	}
	var a BatesAllocation
	err = productionservice.ReserveAfterPreparedInput(stored.Authority, productionservice.PreparedInputReference{
		OperationID:    stored.Authority.Audit.OperationID,
		PreparedSHA256: stored.Authority.Prepared.SHA256,
		ReceiptSHA256:  stored.Authority.Receipt.SHA256,
	}, func(documentproduction.PreparedInputAuthority) error {
		var reserveErr error
		a, reserveErr = s.ReserveBatesRange(ctx, BatesPlanRequest{
			OperationID: job.ID, NamespaceID: namespaceID, SnapshotID: snapshotID,
			RecipeSHA256: recipeSHA, StartAt: startAt, Pages: pages,
		})
		return reserveErr
	})
	if err != nil {
		return documentproduction.NumberReservation{}, err
	}
	numbers := make([]documentproduction.AssignedNumber, 0, len(a.Labels))
	for _, label := range a.Labels {
		numbers = append(numbers, documentproduction.AssignedNumber{MemberID: label.OccurrenceID, MemberOrdinal: int64(label.Ordinal), Page: label.SourcePage, Text: label.Label})
	}
	r := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1, Authority: "bates-ledger/v1", ID: a.AllocationID, OperationID: job.ID, RevisionSHA256: finalized.Authority.Prepared.SHA256, State: a.State, Numbers: numbers}
	_, r.SHA256, err = documentproduction.CanonicalNumberReservation(r)
	if err != nil {
		return documentproduction.NumberReservation{}, fmt.Errorf("canonical reservation: %w", err)
	}
	return r, nil
}

// CancelProductionJob is idempotent and prevents a queued or running job from
// being published after cancellation.
func (s *Store) CancelProductionJob(ctx context.Context, jobID string) error {
	if validateUUIDv4(jobID) != nil {
		return productionservice.ErrJobConflict
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM production_jobs WHERE job_id=?`, jobID).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state == productionservice.ProductionJobSucceeded {
			return productionservice.ErrJobConflict
		}
		_, err := tx.ExecContext(ctx, `UPDATE production_jobs SET state=?,cancel_requested=1,updated_at=? WHERE job_id=?`, productionservice.ProductionJobCanceled, nowRFC3339(), jobID)
		return err
	})
}

func (s *Store) PublishProductionJob(ctx context.Context, claim productionservice.JobClaim, job productionservice.Job, receipt documentproduction.ProductionReceipt, manifest documentproduction.ArtifactManifest, endorsements []redaction.Endorsement) (productionservice.Job, error) {
	if claim.JobID != job.ID || claim.Token == "" || claim.Epoch < 1 {
		return productionservice.Job{}, productionservice.ErrJobStaleClaim
	}
	if err := documentproduction.ValidateProductionReceipt(receipt); err != nil {
		return productionservice.Job{}, err
	}
	if err := documentproduction.ValidateArtifactManifest(manifest); err != nil {
		return productionservice.Job{}, err
	}
	_, reservationSHA, err := documentproduction.CanonicalNumberReservation(job.Reservation)
	if err != nil || receipt.NumberReservationSHA256 != reservationSHA || receipt.RevisionSHA256 != job.RevisionSHA256 || receipt.JobID != job.ID || receipt.SetID != job.SetID || receipt.Revision != job.Revision || receipt.PreparedInputSHA256 != job.PreparedInputSHA256 || receipt.ArtifactManifestSHA256 != manifest.SHA256 {
		return productionservice.Job{}, productionservice.ErrJobConflict
	}
	if len(manifest.Artifacts) == 0 {
		return productionservice.Job{}, productionservice.ErrJobIncomplete
	}
	endorsementRaw, err := canonical.Marshal(endorsements)
	if err != nil {
		return productionservice.Job{}, err
	}
	endorsementDigest := sha256.Sum256(endorsementRaw)
	if receipt.EndorsementsSHA256 != hex.EncodeToString(endorsementDigest[:]) {
		return productionservice.Job{}, productionservice.ErrJobConflict
	}
	_ = endorsementRaw
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var state, token, owner string
		var epoch int64
		var canceled int
		if err := tx.QueryRowContext(ctx, `SELECT state,claim_epoch,claim_token,claim_owner,cancel_requested FROM production_jobs WHERE job_id=?`, job.ID).Scan(&state, &epoch, &token, &owner, &canceled); err != nil {
			return err
		}
		if state == productionservice.ProductionJobCanceled || canceled != 0 || epoch != claim.Epoch || token != claim.Token || owner != claim.Worker {
			return productionservice.ErrJobStaleClaim
		}
		var lease sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT lease_expires_at FROM production_jobs WHERE job_id=?`, job.ID).Scan(&lease); err != nil {
			return err
		}
		if !lease.Valid {
			return productionservice.ErrJobStaleClaim
		}
		if expires, err := time.Parse(time.RFC3339Nano, lease.String); err != nil || !expires.After(time.Now().UTC()) {
			return productionservice.ErrJobStaleClaim
		}
		rows, err := tx.QueryContext(ctx, `SELECT artifact_id,artifact_json FROM production_job_artifacts WHERE job_id=? ORDER BY artifact_id`, job.ID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var staged []documentproduction.Artifact
		for rows.Next() {
			var id string
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				return err
			}
			a, err := canonical.Decode[documentproduction.Artifact](raw)
			if err != nil || a.ID != id {
				return productionservice.ErrJobIncomplete
			}
			staged = append(staged, a)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		stagedManifest := documentproduction.ArtifactManifest{Contract: documentproduction.ArtifactManifestContractV1, Artifacts: staged}
		_, stagedManifest.SHA256, err = documentproduction.CanonicalArtifactManifest(stagedManifest)
		if err != nil || stagedManifest.SHA256 != manifest.SHA256 {
			return productionservice.ErrJobIncomplete
		}
		rawReceipt, _ := canonical.Marshal(receipt)
		rawManifest, _ := canonical.Marshal(manifest)
		rawEndorsements, _ := canonical.Marshal(endorsements)
		if _, err := tx.ExecContext(ctx, `UPDATE production_jobs SET state=?,receipt_sha256=?,receipt_json=?,artifact_manifest_json=?,endorsements_json=?,updated_at=? WHERE job_id=?`, productionservice.ProductionJobSucceeded, receipt.SHA256, rawReceipt, rawManifest, rawEndorsements, nowRFC3339(), job.ID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return productionservice.Job{}, err
	}
	job.State = productionservice.ProductionJobSucceeded
	job.Receipt = receipt
	job.Manifest = manifest
	job.Endorsements = endorsements
	return job, nil
}

// StageProductionArtifact persists each validated artifact before publication;
// publication can therefore atomically refuse an incomplete staged set.
func (s *Store) StageProductionArtifact(ctx context.Context, claim productionservice.JobClaim, artifact documentproduction.Artifact) error {
	if validateUUIDv4(claim.JobID) != nil || claim.Token == "" || artifact.ID == "" || !canonical.IsSHA256Hex(artifact.SHA256) || artifact.Size < 0 {
		return productionservice.ErrJobConflict
	}
	if _, _, err := documentproduction.CanonicalArtifactManifest(documentproduction.ArtifactManifest{Contract: documentproduction.ArtifactManifestContractV1, Artifacts: []documentproduction.Artifact{artifact}}); err != nil {
		return err
	}
	raw, err := canonical.Marshal(artifact)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var epoch int64
		var token, owner, state string
		var canceled int
		if err := tx.QueryRowContext(ctx, `SELECT claim_epoch,claim_token,claim_owner,state,cancel_requested FROM production_jobs WHERE job_id=?`, claim.JobID).Scan(&epoch, &token, &owner, &state, &canceled); err != nil {
			return err
		}
		if epoch != claim.Epoch || token != claim.Token || owner != claim.Worker || canceled != 0 || state == productionservice.ProductionJobCanceled {
			return productionservice.ErrJobStaleClaim
		}
		var prior []byte
		err = tx.QueryRowContext(ctx, `SELECT artifact_json FROM production_job_artifacts WHERE job_id=? AND artifact_id=?`, claim.JobID, artifact.ID).Scan(&prior)
		if err == nil {
			if !bytes.Equal(prior, raw) {
				return productionservice.ErrJobConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_job_artifacts(job_id,artifact_id,artifact_json,created_at) VALUES(?,?,?,?)`, claim.JobID, artifact.ID, raw, nowRFC3339())
		return err
	})
}

// ProductionJobAdapter is the J-owned persistence seam. The coordinator's
// schema checkpoint supplies these transactional callbacks; J keeps all
// authority and lifecycle checks in the service-facing contract.
type ProductionJobAdapter struct {
	Load    func(context.Context, string, int64) (productionservice.FinalizedProduction, error)
	Admit   func(context.Context, productionservice.JobRequest) (productionservice.Job, error)
	Claim   func(context.Context, string, string, time.Duration) (productionservice.JobClaim, error)
	Reserve func(context.Context, productionservice.Job, productionservice.FinalizedProduction) (documentproduction.NumberReservation, error)
	Publish func(context.Context, productionservice.JobClaim, productionservice.Job, documentproduction.ProductionReceipt, documentproduction.ArtifactManifest, []redaction.Endorsement) (productionservice.Job, error)
	Cancel  func(context.Context, string) error
}

func (a ProductionJobAdapter) LoadFinalizedProduction(ctx context.Context, setID string, revision int64) (productionservice.FinalizedProduction, error) {
	return a.Load(ctx, setID, revision)
}
func (a ProductionJobAdapter) AdmitProductionJob(ctx context.Context, r productionservice.JobRequest) (productionservice.Job, error) {
	return a.Admit(ctx, r)
}
func (a ProductionJobAdapter) ClaimProductionJob(ctx context.Context, id, worker string, lease time.Duration) (productionservice.JobClaim, error) {
	return a.Claim(ctx, id, worker, lease)
}
func (a ProductionJobAdapter) ReserveProductionJobNumbers(ctx context.Context, j productionservice.Job, f productionservice.FinalizedProduction) (documentproduction.NumberReservation, error) {
	return a.Reserve(ctx, j, f)
}
func (a ProductionJobAdapter) PublishProductionJob(ctx context.Context, c productionservice.JobClaim, j productionservice.Job, r documentproduction.ProductionReceipt, m documentproduction.ArtifactManifest, e []redaction.Endorsement) (productionservice.Job, error) {
	return a.Publish(ctx, c, j, r, m, e)
}
func (a ProductionJobAdapter) CancelProductionJob(ctx context.Context, id string) error {
	return a.Cancel(ctx, id)
}
